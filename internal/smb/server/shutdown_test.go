package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

type blockedCleanup struct {
	*cleanupStorage
	entered chan struct{}
	release chan struct{}
}

func (storage *blockedCleanup) Close(ctx context.Context, handle smb.Handle) error {
	close(storage.entered)
	<-storage.release
	if err := ctx.Err(); err != nil {
		return err
	}
	return storage.cleanupStorage.Close(ctx, handle)
}

func TestShutdownCleanupContinuesAfterCallerCancellation(t *testing.T) {
	options := testOptions(t)
	storage := &blockedCleanup{cleanupStorage: &cleanupStorage{}, entered: make(chan struct{}), release: make(chan struct{})}
	options.Storage = storage
	object := smb.ObjectKey{Inode: 2}
	reservation, status := options.State.Reserve(state.OpenRequest{Object: object, Binding: state.Binding{SessionID: 1, TreeID: 1}, GrantedAccess: 1, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	if _, status := options.State.Commit(reservation, state.Grant{Handle: cleanupHandle{object: object}}); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(ctx) }()
	select {
	case <-storage.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown never started cleanup")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown caller did not return")
	}
	close(storage.release)
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if storage.closed.Load() != 1 {
		t.Fatalf("cleanup count %d", storage.closed.Load())
	}
}
