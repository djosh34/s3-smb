package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type closingStorage struct {
	pathOf func(context.Context, smb.Inode) (string, error)
	lookup func(context.Context, string) (smb.Resolved, error)
	cleanupStorage
}

func (storage *closingStorage) PathOf(ctx context.Context, inode smb.Inode) (string, error) {
	return storage.pathOf(ctx, inode)
}

func (storage *closingStorage) Lookup(ctx context.Context, path string) (smb.Resolved, error) {
	return storage.lookup(ctx, path)
}

type closeNamespaceFailure struct {
	pathErr    error
	lookupErr  error
	wantErr    error
	name       string
	failAt     int
	mismatch   bool
	persistent bool
}

func TestCloseOpenNamespaceFailuresReleaseResources(t *testing.T) {
	for _, test := range []closeNamespaceFailure{
		{name: "identity discovery race", pathErr: smb.ErrIdentityChanged},
		{name: "moved ancestor during discovery", pathErr: smb.ErrPathNotFound},
		{name: "unlinked inode", pathErr: smb.ErrNameNotFound, persistent: true},
		{name: "moved ancestor before first lookup", lookupErr: smb.ErrPathNotFound, failAt: 1},
		{name: "identity changed under guard", lookupErr: smb.ErrIdentityChanged, failAt: 2},
		{name: "name disappeared under guard", lookupErr: smb.ErrNameNotFound, failAt: 2},
		{name: "selected inode mismatch", mismatch: true, failAt: 2},
		{name: "persistent discovery failure", pathErr: smb.ErrIO, persistent: true, wantErr: smb.ErrIO},
		{name: "persistent first lookup failure", lookupErr: smb.ErrIO, failAt: 1, persistent: true, wantErr: smb.ErrIO},
		{name: "persistent guarded lookup failure", lookupErr: smb.ErrIO, failAt: 2, persistent: true, wantErr: smb.ErrIO},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions(t)
			storage := &closingStorage{}
			options.Storage = storage
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			injectCloseNamespaceFailure(t, server, storage, test)
			open := reserveCleanupOpen(t, server, false)
			request := openRequestContext(server, open)
			if status := request.Opens.Lock(open.ID, request.Binding(), []state.Range{{Offset: 0, Length: 1, Exclusive: true}}, false); status != smb.StatusSuccess {
				t.Fatal(status)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err = closeOpen(ctx, request, open.ID)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("close: %v, want %v", err, test.wantErr)
			}
			if _, status := request.Opens.Find(open.ID, request.Binding()); status != smb.StatusFileClosed {
				t.Fatalf("open retained: %#x", status)
			}
			if storage.closed.Load() != 1 || len(server.parents) != 0 {
				t.Fatalf("closes %d, guards %d", storage.closed.Load(), len(server.parents))
			}
			// A new exclusive reservation proves that the sharing intent was removed.
			token, status := request.Opens.Reserve(state.OpenRequest{Object: open.Object, Binding: request.Binding(), GrantedAccess: 3})
			if status != smb.StatusSuccess {
				t.Fatalf("sharing retained: %#x", status)
			}
			if status := request.Opens.Abort(token); status != smb.StatusSuccess {
				t.Fatal(status)
			}
			if err := closeOpen(ctx, request, open.ID); !errors.Is(err, smb.ErrInvalidHandle) {
				t.Fatalf("second close: %v", err)
			}
			if storage.closed.Load() != 1 {
				t.Fatal("storage handle closed twice")
			}
		})
	}
}

func injectCloseNamespaceFailure(t *testing.T, server *Server, storage *closingStorage, test closeNamespaceFailure) {
	t.Helper()
	pathCalls, lookupCalls := 0, 0
	storage.pathOf = func(ctx context.Context, inode smb.Inode) (string, error) {
		pathCalls++
		if len(server.parents) != 0 {
			t.Error("discovery retried with a parent guard held")
		}
		if test.pathErr != nil && (pathCalls == 1 || test.persistent) {
			return "", test.pathErr
		}
		return storage.cleanupStorage.PathOf(ctx, inode)
	}
	storage.lookup = func(ctx context.Context, path string) (smb.Resolved, error) {
		lookupCalls++
		if test.lookupErr != nil && (lookupCalls == test.failAt || test.persistent && lookupCalls >= test.failAt) {
			return smb.Resolved{}, test.lookupErr
		}
		resolved, err := storage.cleanupStorage.Lookup(ctx, path)
		if test.mismatch && lookupCalls == test.failAt {
			resolved.Object.Inode++
		}
		return resolved, err
	}
}

func reserveCleanupOpen(t *testing.T, server *Server, deleteOnClose bool) state.Open {
	t.Helper()
	object := smb.ObjectKey{Inode: 2}
	token, status := server.options.State.Reserve(state.OpenRequest{Object: object, Binding: state.Binding{SessionID: 1, TreeID: 2}, GrantedAccess: 0x10003, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(token, state.Grant{Handle: cleanupHandle{object: object}, DeleteOnClose: deleteOnClose, DeleteName: smb.Name{Parent: 1, Base: "renamed"}})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

func TestCleanupCancellationDuringDrainStillDeletes(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	open := reserveCleanupOpen(t, server, true)
	request := openRequestContext(server, open)
	_, release, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	action, status := request.Opens.Close(open.ID, request.Binding())
	if status != smb.StatusSuccess || !action.Remove {
		release()
		t.Fatalf("close: %#x, %+v", status, action)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- request.Cleanup(ctx, []state.CloseAction{action}) }()
	assertCleanupWaiting(t, done, storage)
	cancel()
	assertCleanupWaiting(t, done, storage)
	release()
	select {
	case err := <-done:
		if err != nil || storage.closed.Load() != 1 || storage.removed.Load() != 1 {
			t.Fatalf("cleanup: %v, closes %d, removes %d", err, storage.closed.Load(), storage.removed.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not drain")
	}
	if len(server.parents) != 0 || len(server.activeOpens) != 0 {
		t.Fatal("cleanup retained guards or references")
	}
}
