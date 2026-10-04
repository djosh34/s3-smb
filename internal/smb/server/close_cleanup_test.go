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
