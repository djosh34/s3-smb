package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestLockParentsCancellationReleasesGuards(t *testing.T) {
	for _, test := range []struct {
		name                string
		first, second, held smb.Inode
	}{
		{name: "first acquisition", first: 1, second: 2, held: 1},
		{name: "second acquisition in sorted order", first: 2, second: 1, held: 2},
		{name: "same parent", first: 1, second: 1, held: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := New(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			request := RequestContext{server: server}
			held, err := lockParent(t.Context(), request, test.held)
			if err != nil {
				t.Fatal(err)
			}
			defer held()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				unlock, err := lockParents(ctx, request, test.first, test.second)
				if unlock != nil {
					unlock()
				}
				done <- err
			}()
			waitParentUsers(t, server, test.held, 2)
			if test.held == 2 {
				waitParentUsers(t, server, 1, 1)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lock: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("parent acquisition ignored cancellation")
			}
			if len(server.parents) != 1 || server.parents[test.held].refs != 1 {
				t.Fatal("cancellation retained a guard or waiter")
			}
		})
	}
}

func TestLockParentRejectsCancelledContext(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	unlock, err := lockParent(ctx, RequestContext{server: server}, 1)
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, context.Canceled) || len(server.parents) != 0 {
		t.Fatalf("lock: %v, guards %d", err, len(server.parents))
	}
}

func TestCloseOpenCancelledGuardWaitKeepsOpen(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	open := reserveCleanupOpen(t, server, false)
	request := openRequestContext(server, open)
	unlock, err := lockParent(t.Context(), request, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- closeOpen(ctx, request, open.ID) }()
	waitParentUsers(t, server, 1, 2)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("close ignored cancellation")
	}
	unlock()
	if _, status := request.Opens.Find(open.ID, request.Binding()); status != smb.StatusSuccess || storage.closed.Load() != 0 {
		t.Fatalf("cancelled request removed open: %#x, closes %d", status, storage.closed.Load())
	}
	if err := closeOpen(t.Context(), request, open.ID); err != nil {
		t.Fatal(err)
	}
	if storage.closed.Load() != 1 || len(server.parents) != 0 {
		t.Fatal("retry did not release resources")
	}
}
