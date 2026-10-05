// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func TestProtectionLimits(t *testing.T) {
	for _, c := range []struct {
		interval, budget time.Duration
		days             int
		ok               bool
	}{
		{time.Hour, time.Minute, 0, false},
		{24*time.Hour - time.Minute, time.Minute, 1, false}, // reaches trash expiry
		{24*time.Hour - time.Minute - time.Nanosecond, time.Minute, 1, true},
		{time.Hour, time.Minute, 106751, true},
		{time.Hour, time.Minute, 106752, false}, // overflows the JuiceFS trash duration
	} {
		if _, err := NewProtection(c.interval, c.budget, c.days); (err == nil) != c.ok {
			t.Errorf("NewProtection(%s, %s, %d) = %v", c.interval, c.budget, c.days, err)
		}
	}
}

func TestProtectionFollowsWallTime(t *testing.T) {
	p, err := NewProtection(time.Hour, time.Minute, 14)
	if err != nil {
		t.Fatal(err)
	}
	clock := newClock()
	p.now = clock.now
	start := clock.now()
	if err = p.protect(start); err != nil {
		t.Fatal(err)
	}
	clock.add(-time.Hour)
	if p.Check() == nil {
		t.Fatal("open while the wall clock is before the backup")
	}
	clock.add(2*time.Hour + time.Minute)
	if p.Check() == nil {
		t.Fatal("open after a suspend past the window")
	}
	if p.protect(clock.now()) == nil {
		t.Fatal("a late backup revived an expired window")
	}
	clock.add(-time.Hour)
	if err = p.Check(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if p.Check() == nil || p.protect(clock.now()) == nil {
		t.Fatal("closed protection reopened")
	}
}

func TestBackupRetriesWithinProtectionWindow(t *testing.T) {
	ctx := context.Background()
	m, _ := newMetadata(t)
	s := newStore(t)
	clock := newClock()
	mgr := newManager(t, m, s, t.TempDir(), clock.now, 10*time.Minute)
	first, err := mgr.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(mgr.opts.StateDir, "backup-receipt.json")
	prior, err := os.ReadFile(filepath.Clean(receiptPath))
	if err != nil {
		t.Fatal(err)
	}
	clock.add(time.Hour)
	s.fail.Store(true)
	var elapsed time.Duration
	var delays []time.Duration
	mgr.wait = func(ctx context.Context, delay time.Duration) error {
		if mgr.opts.Protection.Check() != nil {
			return errors.New("failed retries closed protection before expiry")
		}
		current, readErr := os.ReadFile(filepath.Clean(receiptPath))
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(prior, current) {
			return errors.New("failed retry replaced the last verified receipt")
		}
		delays = append(delays, delay)
		elapsed += delay
		clock.add(delay)
		if elapsed >= 5*time.Minute {
			s.fail.Store(false)
		}
		return ctx.Err()
	}
	next, err := mgr.Backup(ctx)
	if err != nil {
		t.Fatal("backup did not recover after a five-minute outage:", err)
	}
	if len(delays) < 6 {
		t.Fatalf("retry delays = %v, want backoff up to the 30-second cap", delays)
	}
	for i, delay := range delays {
		want := 30 * time.Second
		if i < 5 {
			want = time.Second << i
		}
		if delay != want {
			t.Fatalf("retry %d delay = %s, want %s", i, delay, want)
		}
	}
	if !next.Snapshot.Equal(clock.now()) || next.Key == first.Key {
		t.Fatalf("recovery did not publish a fresh snapshot: %+v", next)
	}
	if err := mgr.opts.Protection.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledRetriesStopAtProtectionExpiry(t *testing.T) {
	ctx := context.Background()
	m, _ := newMetadata(t)
	s := newStore(t)
	clock := newClock()
	mgr := newManager(t, m, s, t.TempDir(), clock.now, time.Minute)
	first, err := mgr.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A delayed schedule has only ten seconds left, not another full budget.
	clock.add(time.Hour + 50*time.Second)
	s.fail.Store(true)
	var delays []time.Duration
	mgr.wait = func(ctx context.Context, delay time.Duration) error {
		if err := mgr.opts.Protection.Check(); err != nil {
			return errors.New("retry closed protection before its deadline")
		}
		delays = append(delays, delay)
		clock.add(delay)
		return ctx.Err()
	}
	if err := mgr.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scheduled backup expiry = %v", err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 3 * time.Second}; !slices.Equal(delays, want) {
		t.Fatalf("retry delays = %v, want %v", delays, want)
	}
	if !clock.now().Equal(first.Snapshot.Add(time.Hour + time.Minute)) {
		t.Fatal("retry loop did not stop at the protection deadline")
	}
	if err := mgr.opts.Protection.Check(); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("expired protection = %v", err)
	}
	s.fail.Store(false)
	if _, err := mgr.Backup(ctx); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("late backup revived expired protection: %v", err)
	}
	if s.puts.Load() != 5 {
		t.Fatalf("upload after expiry: %d uploads", s.puts.Load())
	}
	if _, err := Inspect(ctx, s, first.Key, t.TempDir()); err != nil {
		t.Fatal("previous backup lost:", err)
	}
}

func TestBackupRetryCancellation(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	mgr := newManager(t, m, s, t.TempDir(), time.Now, time.Minute)
	s.fail.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.wait = func(ctx context.Context, delay time.Duration) error {
		cancel()
		return waitForRetry(ctx, delay)
	}
	if _, err := mgr.Backup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry = %v", err)
	}
	mgr.Wait()
	if s.puts.Load() != 1 {
		t.Fatalf("retry continued after cancellation: %d uploads", s.puts.Load())
	}
}

// heldReadbackStore blocks the readback after an upload until released.
type heldReadbackStore struct {
	*fixtureStore
	started chan struct{}
	release chan struct{}
	hold    atomic.Bool
}

func (s *heldReadbackStore) Get(ctx context.Context, key string, off, limit int64, attrs ...object.AttrGetter) (io.ReadCloser, error) {
	if s.hold.CompareAndSwap(true, false) {
		close(s.started)
		<-s.release
	}
	return s.fixtureStore.Get(ctx, key, off, limit, attrs...)
}

// A backup still verifying when its deadline passes by wall time must fail,
// close protection and keep the last receipt. Wait joins its worker.
func TestBackupPastDeadline(t *testing.T) {
	m, _ := newMetadata(t)
	s := &heldReadbackStore{fixtureStore: newStore(t), started: make(chan struct{}), release: make(chan struct{})}
	clock := newClock()
	mgr := newManager(t, m, s, t.TempDir(), clock.now, time.Minute)
	if _, err := mgr.Backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(mgr.opts.StateDir, "backup-receipt.json")
	prior, err := os.ReadFile(filepath.Clean(receiptPath))
	if err != nil {
		t.Fatal(err)
	}
	clock.add(time.Hour + time.Minute - 2*time.Second)
	s.hold.Store(true)
	done := make(chan error, 1)
	go func() {
		_, backupErr := mgr.Backup(context.Background())
		done <- backupErr
	}()
	<-s.started
	clock.add(3 * time.Second)
	if err = <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late verification = %v, want deadline exceeded", err)
	}
	select {
	case mgr.busy <- struct{}{}:
		t.Fatal("worker released before its verification ended")
	default:
	}
	close(s.release)
	mgr.Wait()
	if err = mgr.opts.Protection.Check(); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("late verification kept protection open: %v", err)
	}
	current, err := os.ReadFile(filepath.Clean(receiptPath))
	if err != nil || !bytes.Equal(prior, current) {
		t.Fatalf("late verification replaced the last receipt: %v", err)
	}
}
