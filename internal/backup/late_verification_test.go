// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

type heldVerificationStore struct {
	*fixtureStore
	hold    atomic.Bool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *heldVerificationStore) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *heldVerificationStore) Get(ctx context.Context, key string, off, limit int64, attrs ...object.AttrGetter) (io.ReadCloser, error) {
	if s.hold.CompareAndSwap(true, false) {
		close(s.started)
		<-s.release
	}
	return s.fixtureStore.Get(ctx, key, off, limit, attrs...)
}

func TestVerificationAfterProtectionExpiryIsRejected(t *testing.T) {
	m, _ := newMetadata(t)
	s := &heldVerificationStore{
		fixtureStore: newStore(t), started: make(chan struct{}), release: make(chan struct{}),
	}
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	mgr := newManager(t, m, s, t.TempDir(), now, time.Minute)
	t.Cleanup(func() { s.unblock(); mgr.Wait() })
	if _, err := mgr.Backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.Wait()
	receiptPath := filepath.Join(mgr.opts.StateDir, "backup-receipt.json")
	prior, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(time.Hour + time.Minute - 2*time.Second))
	s.hold.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Backup(context.Background())
		done <- err
	}()
	select {
	case <-s.started:
	case <-time.After(time.Second):
		t.Fatal("readback verification did not start")
	}
	clock.Add(int64(3 * time.Second))
	s.unblock()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late verification = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backup did not reject late verification")
	}
	mgr.Wait()
	if err := mgr.opts.Protection.Check(); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("late verification revived protection: %v", err)
	}
	current, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prior, current) {
		t.Fatal("late verification replaced the last accepted receipt")
	}
}
