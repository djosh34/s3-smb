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

// Options configures a Manager. Protection must use the same interval, and its
// budget must cover Timeout.
type Options struct {
	Protection        *Protection
	StateDir          string
	DatabasePath      string
	Interval, Timeout time.Duration // Timeout bounds startup backups and receipt verification.
}

// Receipt records a backup that was published and read back from S3.
type Receipt struct {
	Snapshot          time.Time // when the snapshot started
	Key, UUID, SHA256 string
}

// Manager takes metadata backups and keeps Protection open while the last one
// is recent.
type Manager struct {
	receipt Receipt
	blob    object.ObjectStorage
	busy    chan struct{}
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
	opts    Options
	mu      sync.Mutex
}

// New prepares the state directory and returns a Manager. The Manager reads
// metadata from opts.DatabasePath, not through m.
func New(m meta.Meta, blob object.ObjectStorage, opts Options) (*Manager, error) {
	if m == nil || blob == nil || opts.StateDir == "" || opts.DatabasePath == "" || opts.Interval <= 0 || opts.Timeout <= 0 || opts.Protection == nil {
		return nil, errors.New("invalid metadata backup options")
	}
	if opts.Protection.interval != opts.Interval || opts.Timeout > opts.Protection.budget {
		return nil, errors.New("backup budget does not match protection")
	}
	for _, d := range []string{opts.StateDir, filepath.Join(opts.StateDir, "backup-staging"), filepath.Join(opts.StateDir, "backup-names")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := cleanupSnapshotStaging(filepath.Join(opts.StateDir, "backup-staging")); err != nil {
		return nil, err
	}
	return &Manager{blob: blob, opts: opts, busy: make(chan struct{}, 1), now: time.Now, wait: waitForRetry}, nil
}

type result struct {
	err error
	r   Receipt
}

// Backup retries until the last verified backup's protection expires, or until
// Timeout at startup. On failure it closes protection. The caller calls Wait
// before closing the database or state lock.
func (m *Manager) Backup(ctx context.Context) (Receipt, error) {
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
	attemptCtx, cancel := context.WithTimeout(ctx, deadline.Sub(now))
	defer cancel()
	select {
	case m.busy <- struct{}{}:
	case <-attemptCtx.Done():
		m.opts.Protection.Close()
		return Receipt{}, attemptCtx.Err()
	}
	done := make(chan result, 1)
	go m.work(ctx, attemptCtx, deadline, !previous.Snapshot.IsZero(), done)
	r, err := m.await(attemptCtx, deadline, done)
	m.mu.Lock()
	defer m.mu.Unlock()
	// A deadline and a successful publication can both be ready. Honor the
	// result accepted by the worker before deciding whether to close protection.
	if err != nil {
		select {
		case res := <-done:
			r, err = res.r, res.err
		default:
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		m.opts.Protection.Close()
		return Receipt{}, err
	}
	return r, nil
}

// work runs one backup while holding busy, so Wait joins it. Retention runs
// after the result is published and has its own budget.
func (m *Manager) work(parent, ctx context.Context, deadline time.Time, retain bool, done chan<- result) {
	defer func() { <-m.busy }()
	r, err := m.attempts(ctx, deadline)
	if err == nil {
		err = m.late(ctx, deadline)
	}
	if err == nil {
		err = m.save(r)
	}
	// Publish protection and the result together, so a deadline cannot
	// close protection after a verified receipt was accepted in time.
	m.mu.Lock()
	if err == nil {
		err = m.late(ctx, deadline)
	}
	if err == nil {
		err = m.opts.Protection.protect(r.Snapshot)
	}
	if err == nil {
		m.receipt = r
	}
	done <- result{err, r}
	m.mu.Unlock()
	// The first backup after startup removes nothing.
	if err == nil && retain {
		cleanupCtx, stop := context.WithTimeout(parent, 10*time.Second)
		defer stop()
		if err := m.cleanup(cleanupCtx, m.now()); err != nil {
			slog.Warn("metadata backup retention deferred", "error", err)
		}
	}
}

// await waits for the worker's result. Go timers do not advance while the host
// is suspended, so it also compares wall time with the deadline each second.
func (m *Manager) await(ctx context.Context, deadline time.Time, done <-chan result) (Receipt, error) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case res := <-done:
			return res.r, res.err
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		case <-tick.C:
			if !m.now().Round(0).Before(deadline) {
				return Receipt{}, context.DeadlineExceeded
			}
		}
	}
}

func (m *Manager) late(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.now().Round(0).Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// Wait joins the backup worker, including retention after a successful backup
// and work left running by a timeout. Call it before closing the database.
func (m *Manager) Wait() { m.busy <- struct{}{}; <-m.busy }

func (m *Manager) attempts(ctx context.Context, deadline time.Time) (Receipt, error) {
	var last error
	backoff := time.Second
	for {
		if err := m.late(ctx, deadline); err != nil {
			return Receipt{}, errors.Join(err, last)
		}
		now := m.now().UTC().Round(0)
		key := "meta/snapshot-" + now.Format("2006-01-02-150405") + ".db.gz"
		var digest string
		err := m.reserve(key)
		if err == nil {
			digest, err = m.snapshot(ctx, key)
		}
		if err == nil {
			var uuid string
			if uuid, err = metadataUUID(ctx, m.opts.DatabasePath); err != nil {
				return Receipt{}, err
			}
			return Receipt{Key: key, UUID: uuid, SHA256: digest, Snapshot: now}, nil
		}
		last = err
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
	f, err := os.OpenFile(filepath.Clean(filepath.Join(dir, filepath.Base(key))), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
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
	_, err = f.Write(b)
	if err = errors.Join(err, f.Sync(), f.Close()); err == nil {
		err = os.Rename(f.Name(), filepath.Join(m.opts.StateDir, "backup-receipt.json"))
	}
	if err != nil {
		return errors.Join(err, os.Remove(f.Name()))
	}
	return syncDir(m.opts.StateDir)
}

// Reuse accepts the last backup of the previous run when it is younger than the
// interval, belongs to this volume and reads back from S3 with the recorded
// hash. The schedule continues from that backup's time. Otherwise it returns
// false and the caller takes a new backup.
func (m *Manager) Reuse(ctx context.Context) (bool, error) {
	b, err := os.ReadFile(filepath.Clean(filepath.Join(m.opts.StateDir, "backup-receipt.json")))
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
	_, err = io.Copy(h, reader)
	if err = errors.Join(err, reader.Close()); err != nil {
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
		// Go timers do not advance while the host is suspended. Wake each second
		// and compare wall time, so a backup due after resume starts at once.
		wait := min(max(r.Snapshot.Add(m.opts.Interval).Sub(m.now().Round(0)), 0), time.Second)
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
