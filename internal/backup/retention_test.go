// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func TestRetentionAndReservationCleanup(t *testing.T) {
	ctx := context.Background()
	m, _ := newMetadata(t)
	s := newStore(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	mgr := newManager(t, m, s, t.TempDir(), func() time.Time { return now }, time.Minute)
	if err := mgr.opts.Protection.protect(now); err != nil {
		t.Fatal(err)
	}
	key := func(age time.Duration) string {
		return "meta/snapshot-" + now.Add(-age).Format("2006-01-02-150405") + ".db.gz"
	}
	names := filepath.Join(mgr.opts.StateDir, "backup-names")
	// The earliest snapshot in each thinning period stays, not the newest.
	kept := []string{key(time.Hour), key(47 * time.Hour), key(71 * time.Hour), key(335 * time.Hour), key(600 * time.Hour), key(2000 * time.Hour)}
	removed := []string{key(49 * time.Hour), key(313 * time.Hour), key(550 * time.Hour), key(1900 * time.Hour), key(3 * 365 * 24 * time.Hour)}
	for _, k := range append(append([]string{}, kept...), removed...) {
		if err := s.Put(ctx, k, bytes.NewReader(nil)); err != nil {
			t.Fatal(err)
		}
		if err := mgr.reserve(k); err != nil {
			t.Fatal(err)
		}
	}
	// Names of failed backups stay for two days.
	oldFailed, recentFailed := key(100*time.Hour), key(2*time.Hour)
	for _, k := range []string{oldFailed, recentFailed} {
		if err := mgr.reserve(k); err != nil {
			t.Fatal(err)
		}
	}
	unknown := filepath.Join(names, "keep-me")
	if err := os.WriteFile(unknown, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.cleanup(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, k := range kept {
		if _, err := s.Head(ctx, k); err != nil {
			t.Fatalf("removed retained snapshot %s: %v", k, err)
		}
	}
	for _, k := range removed {
		if _, err := s.Head(ctx, k); !os.IsNotExist(err) {
			t.Fatalf("kept expired snapshot %s: %v", k, err)
		}
	}
	for _, k := range append(removed, oldFailed) {
		if _, err := os.Stat(filepath.Join(names, filepath.Base(k))); !os.IsNotExist(err) {
			t.Fatalf("kept expired name %s: %v", k, err)
		}
	}
	for _, path := range []string{unknown, filepath.Join(names, filepath.Base(recentFailed)), filepath.Join(names, filepath.Base(kept[5]))} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	mgr.opts.Protection.Close()
	if err := mgr.cleanup(ctx, now.AddDate(3, 0, 0)); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("retention ignored protection: %v", err)
	}
}

// heldListStore blocks the retention listing until released.
type heldListStore struct {
	*fixtureStore
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	hold     atomic.Bool
}

func (s *heldListStore) List(ctx context.Context, prefix, startAfter, token, delimiter string, limit int64, followLink bool) ([]object.Object, bool, string, error) {
	if s.hold.CompareAndSwap(true, false) {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
			close(s.canceled)
			<-s.release
			return nil, false, "", ctx.Err()
		}
	}
	return s.fixtureStore.List(ctx, prefix, startAfter, token, delimiter, limit, followLink)
}

// Retention is optional. A verified backup renews protection before retention
// starts, and retention running past the old deadline does not close it.
func TestSlowRetentionKeepsRenewedProtection(t *testing.T) {
	m, _ := newMetadata(t)
	s := &heldListStore{fixtureStore: newStore(t), started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	clock := newClock()
	mgr := newManager(t, m, s, t.TempDir(), clock.now, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := mgr.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	clock.add(time.Hour + time.Minute - 2*time.Second)
	s.hold.Store(true)
	next, err := mgr.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-s.started
	clock.add(3 * time.Second)
	if err = mgr.opts.Protection.Check(); err != nil {
		t.Fatal("retention past the old deadline closed renewed protection:", err)
	}
	data, err := os.ReadFile(filepath.Clean(filepath.Join(mgr.opts.StateDir, "backup-receipt.json")))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Receipt
	if err := json.Unmarshal(data, &persisted); err != nil || persisted != next {
		t.Fatalf("durable receipt = %+v, want %+v: %v", persisted, next, err)
	}
	cancel()
	<-s.canceled
	select {
	case mgr.busy <- struct{}{}:
		t.Fatal("worker released before retention ended")
	default:
	}
	close(s.release)
	mgr.Wait()
}
