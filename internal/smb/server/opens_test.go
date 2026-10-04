package server

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func openRequestContext(server *Server, open state.Open) RequestContext {
	return RequestContext{
		server: server, Storage: server.options.Storage, Opens: server.options.State,
		Session: Session{SessionID: open.Binding.SessionID}, Tree: Tree{TreeID: open.Binding.TreeID},
	}
}

func TestUseOpenValidatesIdentity(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	open := insertSessionOpen(t, server, smbtest.Session{SessionID: 1, TreeID: 2}, false, 3)
	request := openRequestContext(server, open)
	found, release, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusSuccess || found != open || release == nil {
		t.Fatalf("use open: %+v, %#x", found, status)
	}
	release()
	for _, change := range []func(*RequestContext, *wire.FileID){
		func(request *RequestContext, _ *wire.FileID) { request.Session.SessionID++ },
		func(request *RequestContext, _ *wire.FileID) { request.Tree.TreeID++ },
		func(_ *RequestContext, id *wire.FileID) { id.Persistent++ },
		func(_ *RequestContext, id *wire.FileID) { id.Volatile++ },
		func(_ *RequestContext, id *wire.FileID) {
			*id = wire.FileID{Persistent: math.MaxUint64, Volatile: math.MaxUint64}
		},
	} {
		request := request
		id := wire.FileID(open.ID)
		change(&request, &id)
		_, release, status := useOpen(request, id)
		if status != smb.StatusFileClosed || release != nil {
			t.Fatalf("invalid identity: %#x, release present %t", status, release != nil)
		}
	}
}

func TestCleanupDrainsOpenUses(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	open := insertSessionOpen(t, server, smbtest.Session{SessionID: 1, TreeID: 2}, false, 3)
	request := openRequestContext(server, open)
	_, first, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	_, second, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusSuccess {
		first()
		t.Fatal(status)
	}
	action, status := request.Opens.Close(open.ID, request.Binding())
	if status != smb.StatusSuccess || action.FileID != open.ID {
		first()
		second()
		t.Fatalf("close: %+v, %#x", action, status)
	}
	done := make(chan error, 1)
	go func() { done <- server.cleanup(context.Background(), []state.CloseAction{action}) }()
	assertCleanupWaiting(t, done, storage)
	first()
	assertCleanupWaiting(t, done, storage)
	_, release, status := useOpen(request, wire.FileID(open.ID))
	if status != smb.StatusFileClosed || release != nil {
		t.Errorf("closing open acquired: %#x", status)
	}
	second()
	select {
	case err := <-done:
		if err != nil || storage.closed.Load() != 1 {
			t.Fatalf("cleanup: %v, closes %d", err, storage.closed.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not drain")
	}
}

func assertCleanupWaiting(t *testing.T, done <-chan error, storage *cleanupStorage) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("cleanup finished with active references: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	if storage.closed.Load() != 0 {
		t.Fatal("storage handle closed with active references")
	}
}
