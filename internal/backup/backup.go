// SPDX-License-Identifier: AGPL-3.0-only
// Package backup schedules JuiceFS metadata backups, removes old ones and loads
// one into a new database.
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
	Interval, Timeout time.Duration // Timeout covers all attempts of one backup.
	Attempts          int
	Protection        *Protection
}
type Receipt struct {
	Key, UUID, SHA256 string
	Snapshot          time.Time // when the export started
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
	}
	// Remove export files left by an earlier run. Links and other names stay.
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

// Backup takes one metadata backup within Timeout. A JuiceFS dump cannot be
// cancelled, so on timeout Backup closes protection and returns while the dump
// may still run. The caller then calls Wait or exits with the state lock held.
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
	go func() {
		defer func() { <-m.busy }()
		r, e := m.attempts(ctx)
		if e == nil {
			e = ctx.Err()
		}
		if e == nil {
			e = m.save(r)
		}
		// Saving the receipt and removing old backups run in this worker, so
		// Wait covers them. Protection is closed during the first backup after
		// startup, so that one removes nothing.
		if e == nil && m.opts.Protection.Check() == nil {
			cleanupCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			if err := vfs.CleanupBackups(cleanupCtx, guardedStore{m.blob, m.opts.Protection}, m.now()); err != nil {
				slog.Warn("metadata backup retention deferred")
			}
			stop()
		}
		done <- result{r, e}
	}()
	var r Receipt
	var err error
	deadline := m.now().Round(0).Add(m.opts.Timeout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
wait:
	for {
		select {
		case result := <-done:
			r, err = result.r, result.err
			break wait
		case <-ctx.Done():
			err = ctx.Err()
			break wait
		case <-tick.C:
			if !m.now().Round(0).Before(deadline) {
				err = context.DeadlineExceeded
				break wait
			}
		}
	}
	if err == nil {
		err = ctx.Err()
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
	return r, nil
}

// Wait blocks until an export left running by a timeout has ended. Call it
// before closing the metadata database.
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
	return Receipt{}, fmt.Errorf("metadata backup failed after %d attempts: %w", m.opts.Attempts, last)
}
func (m *Manager) reserve(key string) error {
	// One empty file per backup name, kept across restarts. With a single
	// writer, this file and the HEAD request keep a name from being used twice,
	// also after an upload whose response was lost.
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

// Reuse accepts the last backup of the previous run when it is younger than the
// interval, belongs to this volume and reads back from S3 with the recorded
// hash. The schedule continues from that backup's time. Otherwise it returns
// false and the caller takes a new backup.
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
	if errors.Is(err, os.ErrNotExist) {
		slog.Warn("last metadata backup is missing from S3; taking a new one", "key", r.Key)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	h := sha256.New()
	_, e := io.Copy(h, reader)
	if err = errors.Join(e, reader.Close()); err != nil {
		return false, err
	}
	if hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		slog.Warn("last metadata backup in S3 differs from the one recorded locally; taking a new one", "key", r.Key)
		return false, nil
	}
	if err = m.opts.Protection.protect(r.Snapshot); err != nil {
		return false, nil
	}
	m.mu.Lock()
	m.receipt = r
	m.mu.Unlock()
	return true, nil
}

// Run takes a backup each interval and returns the first failure. It closes
// protection when it returns, also on cancellation. The caller stops SMB.
func (m *Manager) Run(ctx context.Context) error {
	defer m.opts.Protection.Close()
	for {
		m.mu.Lock()
		r := m.receipt
		m.mu.Unlock()
		if r.Snapshot.IsZero() {
			return ErrUnprotected
		}
		if err := m.opts.Protection.Check(); err != nil {
			return err
		}
		wait := r.Snapshot.Add(m.opts.Interval).Sub(m.now().Round(0))
		if wait < 0 {
			wait = 0
		}
		// Go timers do not advance while the host is suspended. Wake each second
		// and compare wall time, so a backup due after resume starts at once.
		if wait > time.Second {
			wait = time.Second
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
		if m.now().Round(0).Before(r.Snapshot.Add(m.opts.Interval)) {
			continue
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
