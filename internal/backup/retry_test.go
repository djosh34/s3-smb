// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackupRetriesAndRecoversWithinProtectionWindow(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	mgr := newManager(t, m, s, t.TempDir(), now, 10*time.Minute)
	first, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(mgr.opts.StateDir, "backup-receipt.json")
	prior, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(time.Hour))
	s.fail.Store(true)
	var elapsed time.Duration
	var delays []time.Duration
	mgr.wait = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := mgr.opts.Protection.Check(); err != nil {
			return errors.New("fast S3 failures closed protection before expiry")
		}
		current, err := os.ReadFile(receiptPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(prior, current) {
			return errors.New("failed retry replaced the last verified receipt")
		}
		delays = append(delays, delay)
		elapsed += delay
		clock.Add(int64(delay))
		if elapsed >= 5*time.Minute {
			s.fail.Store(false)
		}
		return nil
	}
	next, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal("backup did not recover after a five-minute outage:", err)
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
	if len(delays) <= 3 || s.puts.Load() != int32(len(delays)+2) {
		t.Fatalf("retry loop ended early: delays=%v uploads=%d", delays, s.puts.Load())
	}
	if !next.Snapshot.Equal(now()) || next.Key == first.Key {
		t.Fatalf("recovery did not publish a fresh snapshot: %+v", next)
	}
	if err := mgr.opts.Protection.Check(); err != nil {
		t.Fatal("successful retry did not retain protection:", err)
	}
	// The same manager can take the next backup without a restart.
	clock.Add(int64(time.Hour))
	if _, err := mgr.Backup(context.Background()); err != nil {
		t.Fatal("next backup failed after recovery:", err)
	}
}

func TestScheduledRetriesStopAtProtectionExpiry(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	mgr := newManager(t, m, s, t.TempDir(), now, time.Minute)
	first, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A delayed schedule has only ten seconds left, not another full budget.
	clock.Add(int64(time.Hour + 50*time.Second))
	s.fail.Store(true)
	var delays []time.Duration
	mgr.wait = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := mgr.opts.Protection.Check(); err != nil {
			return errors.New("retry closed protection before its deadline")
		}
		delays = append(delays, delay)
		clock.Add(int64(delay))
		return nil
	}
	if err := mgr.Run(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scheduled backup expiry = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 3 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("retry delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("retry delays = %v, want %v", delays, want)
		}
	}
	if !now().Equal(first.Snapshot.Add(time.Hour + time.Minute)) {
		t.Fatal("retry loop did not stop at the protection deadline")
	}
	if err := mgr.opts.Protection.Check(); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("expired protection = %v", err)
	}
	s.fail.Store(false)
	if _, err := mgr.Backup(context.Background()); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("late backup revived expired protection: %v", err)
	}
	if s.puts.Load() != 5 {
		t.Fatalf("upload after expiry: %d uploads", s.puts.Load())
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

func TestProtectionRejectsLateBackup(t *testing.T) {
	p, err := NewProtection(time.Hour, time.Minute, 14)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.now = func() time.Time { return now }
	if err := p.protect(now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour + time.Minute)
	if err := p.protect(now); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("late backup revived expired protection: %v", err)
	}
}
