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

func TestUseOpenResolvesRelatedFileID(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	open := insertSessionOpen(t, server, smbtest.Session{SessionID: 1, TreeID: 2}, false, 3)
	request := openRequestContext(server, open)
	request.related, request.fileID = true, wire.FileID(open.ID)
	placeholder := wire.FileID{Persistent: math.MaxUint64, Volatile: math.MaxUint64}
	found, release, status := useOpen(request, placeholder)
	if status != smb.StatusSuccess || found.ID != open.ID {
		t.Fatalf("inherited open: %+v, %#x", found, status)
	}
	release()
	request.fileID = wire.FileID{}
	_, release, status = useOpen(request, placeholder)
	if status != smb.StatusInvalidParameter || release != nil {
		t.Fatalf("missing inherited open: %#x", status)
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

func TestReconnectKeepsReferencesAcrossVolatileIDs(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	open := insertSessionOpen(t, server, smbtest.Session{SessionID: 1, TreeID: 2}, true, 3)
	_, release, status := useOpen(openRequestContext(server, open), wire.FileID(open.ID))
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	defer release()
	if actions := options.State.Disconnect(open.Binding.SessionID); len(actions) != 0 {
		t.Fatal("durable open closed on drop")
	}
	attached, status := options.State.Reconnect(state.ReconnectRequest{
		ID: open.ID, Binding: state.Binding{SessionID: 4, TreeID: 5},
		User: open.User, Share: open.Share, ClientGUID: open.ClientGUID,
		CreateGUID: open.CreateGUID, LeaseKey: open.LeaseKey,
	})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	_, newRelease, status := useOpen(openRequestContext(server, attached), wire.FileID(attached.ID))
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	newRelease()
	server.openMu.Lock()
	uses := server.activeOpens[open.ID.Persistent]
	if uses == nil || uses.count != 1 {
		t.Error("old binding's reference was not preserved")
	}
	server.openMu.Unlock()
}

func TestSessionCleanupWaitsForOpenReference(t *testing.T) {
	for _, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
		t.Run(map[wire.Command]string{wire.Logoff: "logoff", wire.TreeDisconnect: "tree disconnect"}[command], func(t *testing.T) {
			options := testOptions(t)
			storage := &cleanupStorage{}
			options.Storage = storage
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
			open := insertSessionOpen(t, server, session, false, 2)
			_, release, status := useOpen(openRequestContext(server, open), wire.FileID(open.ID))
			if status != smb.StatusSuccess {
				t.Fatal(status)
			}
			if err := client.Send(ctx, []wire.Message{treeRequest(t, session, session.NextMessageID, command)}); err != nil {
				release()
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				response, receiveErr := client.Receive(ctx)
				if receiveErr == nil && response.Messages[0].Header.Status != smb.StatusSuccess {
					t.Errorf("cleanup reply: %+v", response.Messages[0].Header)
				}
				done <- receiveErr
			}()
			select {
			case err := <-done:
				release()
				t.Fatalf("cleanup replied with active reference: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if storage.closed.Load() != 0 {
				t.Error("session cleanup closed an active handle")
			}
			release()
			select {
			case err := <-done:
				if err != nil || storage.closed.Load() != 1 {
					t.Fatalf("cleanup: %v, handles closed %d", err, storage.closed.Load())
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
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
