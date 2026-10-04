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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

type Options struct {
	StateDir          string
	DatabasePath      string
	Interval, Timeout time.Duration // Timeout bounds startup backups and receipt verification.
	Protection        *Protection
}
type Receipt struct {
	Key, UUID, SHA256 string
	Snapshot          time.Time // when the snapshot started
}
type Manager struct {
	blob    object.ObjectStorage
	opts    Options
	mu      sync.Mutex
	receipt Receipt
	busy    chan struct{}
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
}

func New(m meta.Meta, blob object.ObjectStorage, opts Options) (*Manager, error) {
	if m == nil || blob == nil || opts.StateDir == "" || opts.DatabasePath == "" || opts.Interval <= 0 || opts.Timeout <= 0 || opts.Protection == nil {
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
	if err := cleanupSnapshotStaging(filepath.Join(opts.StateDir, "backup-staging")); err != nil {
		return nil, err
	}
	return &Manager{blob: blob, opts: opts, busy: make(chan struct{}, 1), now: time.Now, wait: waitForRetry}, nil
}

// Backup retries until the last verified backup's protection expires, or until
// Timeout at startup. On failure it closes protection. The caller calls Wait
// before closing the database or state lock.
func (m *Manager) Backup(ctx context.Context) (Receipt, error) {
	parent := ctx
	m.mu.Lock()
	previous := m.receipt
	m.mu.Unlock()
	now := m.now().Round(0)
	deadline := now.Add(m.opts.Timeout)
	if !previous.Snapshot.IsZero() {
		if err := m.opts.Protection.Check(); err != nil {
			m.opts.Protection.Close()
			return Receipt{}, err
		}
		deadline = previous.Snapshot.Add(m.opts.Interval + m.opts.Protection.budget)
	}
	ctx, cancel := context.WithTimeout(ctx, deadline.Sub(now))
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
		r, e := m.attempts(ctx, deadline)
		if e == nil {
			e = ctx.Err()
		}
		if e == nil && !m.now().Round(0).Before(deadline) {
			e = context.DeadlineExceeded
		}
		if e == nil {
			e = m.save(r)
		}
		// Publish protection and the result together, so a deadline cannot
		// close protection after a verified receipt was accepted in time.
		m.mu.Lock()
		if e == nil {
			e = ctx.Err()
		}
		if e == nil && !m.now().Round(0).Before(deadline) {
			e = context.DeadlineExceeded
		}
		if e == nil {
			e = m.opts.Protection.protect(r.Snapshot)
		}
		if e == nil {
			m.receipt = r
		}
		done <- result{r, e}
		m.mu.Unlock()
		// Retention is optional and uses its own budget, not the old protection
		// deadline. The worker stays busy until it ends, so Wait still joins it.
		// The first backup after startup removes nothing.
		if e == nil && !previous.Snapshot.IsZero() {
			cleanupCtx, stop := context.WithTimeout(parent, 10*time.Second)
			if err := m.cleanup(cleanupCtx, m.now()); err != nil {
				slog.Warn("metadata backup retention deferred", "error", err)
			}
			stop()
		}
	}()
	var r Receipt
	var err error
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
	m.mu.Lock()
	// A deadline and a successful publication can both be ready. Honor the
	// result accepted by the worker before deciding whether to close protection.
	if err != nil {
		select {
		case result := <-done:
			r, err = result.r, result.err
		default:
		}
	}
	if err == nil {
		err = parent.Err()
	}
	if err != nil {
		m.opts.Protection.Close()
	}
	m.mu.Unlock()
	if err != nil {
		return Receipt{}, err
	}
	return r, nil
}

// Wait joins the backup worker, including retention after a successful backup
// and work left running by a timeout. Call it before closing the database.
func (m *Manager) Wait() { m.busy <- struct{}{}; <-m.busy }

func (m *Manager) attempts(ctx context.Context, deadline time.Time) (Receipt, error) {
	var last error
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return Receipt{}, errors.Join(err, last)
		}
		now := m.now().UTC().Round(0)
		if !now.Before(deadline) {
			return Receipt{}, errors.Join(context.DeadlineExceeded, last)
		}
		key := "meta/snapshot-" + now.Format("2006-01-02-150405") + ".db.gz"
		if err := m.reserve(key); err != nil {
			last = err
		} else {
			digest, err := m.snapshot(ctx, key)
			if err == nil {
				uuid, e := metadataUUID(ctx, m.opts.DatabasePath)
				if e != nil {
					return Receipt{}, e
				}
				return Receipt{Key: key, UUID: uuid, SHA256: digest, Snapshot: now}, nil
			}
			last = err
		}
		delay := min(backoff, deadline.Sub(m.now().Round(0)))
		if delay <= 0 {
			return Receipt{}, errors.Join(context.DeadlineExceeded, last)
		}
		slog.Warn("metadata backup failed; retrying", "error", last, "retry_in", delay)
		if err := m.wait(ctx, delay); err != nil {
			return Receipt{}, errors.Join(err, last)
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	uuid, err := metadataUUID(ctx, m.opts.DatabasePath)
	if err != nil {
		return false, err
	}
	if r.UUID != uuid {
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

// Run takes a backup each interval, retrying failures while protection is valid.
// It closes protection when it returns, also on cancellation. The caller stops SMB.
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
