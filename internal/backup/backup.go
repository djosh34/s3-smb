// SPDX-License-Identifier: AGPL-3.0-only
// Package backup coordinates native JuiceFS metadata export, retention and load.
// It does not define a second remote backup format.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
)

type Options struct {
	StateDir          string
	Interval, Timeout time.Duration // Timeout is the total budget, including retries.
	Attempts          int
	Protection        *Protection
}
type Receipt struct {
	Key, UUID, SHA256 string
	Snapshot          time.Time // start of export, never completion or restart time
}
type Manager struct {
	meta    meta.Meta
	blob    object.ObjectStorage
	opts    Options
	mu      sync.Mutex
	receipt Receipt
	busy    chan struct{}
	now     func() time.Time
}

func New(m meta.Meta, blob object.ObjectStorage, opts Options) (*Manager, error) {
	if m == nil || blob == nil || opts.StateDir == "" || opts.Interval <= 0 || opts.Timeout <= 0 || opts.Attempts < 1 || opts.Protection == nil {
		return nil, errors.New("invalid metadata backup options")
	}
	if opts.Protection.interval != opts.Interval || opts.Timeout > opts.Protection.budget {
		return nil, errors.New("backup budget does not match protection")
	}
	for _, d := range []string{opts.StateDir, filepath.Join(opts.StateDir, "backup-staging"), filepath.Join(opts.StateDir, "backup-names")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
		st, err := os.Stat(d)
		if err != nil {
			return nil, err
		}
		if st.Mode().Perm()&0077 != 0 {
			slog.Warn("existing backup directory permissions are not private")
		}
	}
	// Only our own export files in this locked state directory; never follow links.
	entries, err := os.ReadDir(filepath.Join(opts.StateDir, "backup-staging"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		ok, _ := filepath.Match("export-*.json.gz", e.Name())
		if ok {
			if err = os.Remove(filepath.Join(opts.StateDir, "backup-staging", e.Name())); err != nil {
				return nil, err
			}
		}
	}
	return &Manager{meta: m, blob: blob, opts: opts, busy: make(chan struct{}, 1), now: time.Now}, nil
}

// Backup bounds the whole operation. Native SQL dump cannot be forcibly canceled:
// on timeout this closes protection, and lifecycle MUST exit within its shutdown
// deadline without releasing the authority lock while a native worker is alive.
func (m *Manager) Backup(ctx context.Context) (Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()
	select {
	case m.busy <- struct{}{}:
	case <-ctx.Done():
		m.opts.Protection.Close()
		return Receipt{}, ctx.Err()
	}
	type result struct {
		r   Receipt
		err error
	}
	done := make(chan result, 1)
	go func() { defer func() { <-m.busy }(); r, e := m.attempts(ctx); done <- result{r, e} }()
	var r Receipt
	var err error
	select {
	case result := <-done:
		r, err = result.r, result.err
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = m.save(r)
	}
	if err == nil {
		err = m.opts.Protection.protect(r.Snapshot)
	}
	if err != nil {
		m.opts.Protection.Close()
		return Receipt{}, err
	}
	m.mu.Lock()
	m.receipt = r
	m.mu.Unlock()
	// Native retention is best-effort only after a verified successful point.
	// Never turn cleanup failure into erasure or loss of the successful receipt.
	cleanupCtx, stop := context.WithTimeout(ctx, time.Second*10)
	defer stop()
	if err = vfs.CleanupBackups(cleanupCtx, guardedStore{m.blob, m.opts.Protection}, m.now()); err != nil {
		slog.Warn("metadata backup retention deferred")
	}
	return r, nil
}

// Wait joins a native export left running by a timeout. Lifecycle must call it
// under its hard shutdown watchdog before closing metadata or releasing lock.
func (m *Manager) Wait() { m.busy <- struct{}{}; <-m.busy }

func (m *Manager) attempts(ctx context.Context) (Receipt, error) {
	var last error
	for i := 0; i < m.opts.Attempts; i++ {
		if err := ctx.Err(); err != nil {
			return Receipt{}, err
		}
		now := m.now().UTC().Round(0)
		key := "meta/dump-" + now.Format("2006-01-02-150405") + ".json.gz"
		if err := m.reserve(key); err != nil {
			last = err
		} else {
			digest, err := vfs.BackupTo(ctx, m.meta, m.blob, filepath.Join(m.opts.StateDir, "backup-staging"), key)
			if err == nil {
				f, e := m.meta.Load(false)
				if e != nil {
					return Receipt{}, e
				}
				return Receipt{Key: key, UUID: f.UUID, SHA256: digest, Snapshot: now}, nil
			}
			last = err
		}
		if i+1 < m.opts.Attempts {
			select {
			case <-ctx.Done():
				return Receipt{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return Receipt{}, fmt.Errorf("metadata backup failed after bounded attempts: %w", last)
}
func (m *Manager) reserve(key string) error {
	// Reservations persist across process restarts and ambiguous uploads. Under
	// the single-writer authority rule, HEAD plus this O_EXCL journal prevents reuse.
	dir := filepath.Join(m.opts.StateDir, "backup-names")
	f, err := os.OpenFile(filepath.Join(dir, filepath.Base(key)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return syncDir(dir)
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
func (m *Manager) save(r Receipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.opts.StateDir, ".backup-receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(m.opts.StateDir, "backup-receipt.json")); err != nil {
		return err
	}
	return syncDir(m.opts.StateDir)
}

// Reuse verifies full remote readback and volume identity. It does not shift the
// original snapshot or schedule. Missing/stale evidence requires a new backup.
func (m *Manager) Reuse(ctx context.Context) (bool, error) {
	b, err := os.ReadFile(filepath.Join(m.opts.StateDir, "backup-receipt.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var r Receipt
	if json.Unmarshal(b, &r) != nil {
		return false, nil
	}
	now := m.now().Round(0)
	if r.Snapshot.IsZero() || r.Snapshot.After(now) || !now.Before(r.Snapshot.Add(m.opts.Interval)) {
		return false, nil
	}
	if _, err = parsePoint(r.Key); err != nil {
		return false, nil
	}
	f, err := m.meta.Load(false)
	if err != nil {
		return false, err
	}
	if r.UUID != f.UUID {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()
	reader, err := m.blob.Get(ctx, r.Key, 0, -1)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	_, e := io.Copy(h, reader)
	if err = errors.Join(e, reader.Close()); err != nil {
		return false, err
	}
	if hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		return false, errors.New("metadata receipt readback mismatch")
	}
	if err = m.opts.Protection.protect(r.Snapshot); err != nil {
		return false, nil
	}
	m.mu.Lock()
	m.receipt = r
	m.mu.Unlock()
	return true, nil
}

// Run returns an error on the first exhausted scheduled operation, and closes
// the maintenance gate even if cancellation was the cause. Caller stops SMB.
func (m *Manager) Run(ctx context.Context) error {
	defer m.opts.Protection.Close()
	for {
		m.mu.Lock()
		r := m.receipt
		m.mu.Unlock()
		if r.Snapshot.IsZero() {
			return ErrUnprotected
		}
		wait := r.Snapshot.Add(m.opts.Interval).Sub(m.now().Round(0))
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if err := m.opts.Protection.Check(); err != nil {
			return err
		}
		if _, err := m.Backup(ctx); err != nil {
			return err
		}
	}
}

type guardedStore struct {
	object.ObjectStorage
	p *Protection
}

func (g guardedStore) Delete(ctx context.Context, key string, attrs ...object.AttrGetter) error {
	if err := g.p.Check(); err != nil {
		return err
	}
	return g.ObjectStorage.Delete(ctx, key, attrs...)
}
