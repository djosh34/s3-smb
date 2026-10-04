// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

type heldRetentionStore struct {
	*fixtureStore
	hold     atomic.Bool
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (s *heldRetentionStore) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *heldRetentionStore) List(ctx context.Context, prefix, startAfter, token, delimiter string, limit int64, followLink bool) ([]object.Object, bool, string, error) {
	if s.hold.CompareAndSwap(true, false) {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
			close(s.canceled)
			// Keep the worker alive after cancellation to check that Wait joins it.
			<-s.release
			return nil, false, "", ctx.Err()
		}
	}
	return s.fixtureStore.List(ctx, prefix, startAfter, token, delimiter, limit, followLink)
}

func TestVerifiedRetryRenewsProtectionBeforeHeldRetention(t *testing.T) {
	m, _ := newMetadata(t)
	s := &heldRetentionStore{
		fixtureStore: newStore(t), started: make(chan struct{}),
		canceled: make(chan struct{}), release: make(chan struct{}),
	}
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	mgr := newManager(t, m, s, t.TempDir(), now, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.unblock(); mgr.Wait() })
	first, err := mgr.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Wait()
	// Verification finishes with two seconds left in the old window.
	clock.Add(int64(time.Hour + time.Minute - 2*time.Second))
	verifiedAt := now()
	s.hold.Store(true)
	type result struct {
		receipt Receipt
		err     error
	}
	done := make(chan result, 1)
	go func() {
		receipt, err := mgr.Backup(ctx)
		done <- result{receipt, err}
	}()
	select {
	case <-s.started:
	case <-time.After(time.Second):
		t.Fatal("retention List did not start")
	}
	// Retention remains blocked while wall time crosses the old deadline.
	clock.Add(int64(3 * time.Second))
	var next Receipt
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal("held retention turned a verified retry into failure:", result.err)
		}
		next = result.receipt
	case <-time.After(2 * time.Second):
		t.Fatal("verified retry waited for optional retention")
	}
	if !next.Snapshot.Equal(verifiedAt) || next.Key == first.Key {
		t.Fatalf("verified retry did not publish a new receipt: %+v", next)
	}
	if err := mgr.opts.Protection.Check(); err != nil {
		t.Fatal("retention crossing the old deadline closed renewed protection:", err)
	}
	data, err := os.ReadFile(filepath.Join(mgr.opts.StateDir, "backup-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Receipt
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != next {
		t.Fatalf("durable receipt = %+v, want %+v", persisted, next)
	}
	select {
	case <-s.canceled:
		t.Fatal("returning a verified receipt canceled optional retention")
	default:
	}
	joined := make(chan struct{})
	go func() { mgr.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("Wait returned while retention was still blocked")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-s.canceled:
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not reach retention")
	}
	select {
	case <-joined:
		t.Fatal("Wait returned before the canceled retention worker ended")
	default:
	}
	s.unblock()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("Wait did not join the released retention worker")
	}
}
