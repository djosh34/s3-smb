package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

type failedCreateCleanupStorage struct {
	smb.Storage
	closeErr  error
	removeErr error
}

func (storage *failedCreateCleanupStorage) Close(ctx context.Context, handle smb.Handle) error {
	return errors.Join(storage.Storage.Close(ctx, handle), storage.closeErr)
}

func (storage *failedCreateCleanupStorage) Remove(ctx context.Context, name smb.Name, inode smb.Inode) error {
	if storage.removeErr != nil {
		return storage.removeErr
	}
	return storage.Storage.Remove(ctx, name, inode)
}

func TestFailedCreateCompletesDeleteOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "close failure", "remove failure", "changed inode", "remove cancelled"} {
		t.Run(outcome, func(t *testing.T) { checkFailedCreateDeletion(t, outcome) })
	}
}

func checkFailedCreateDeletion(t *testing.T, outcome string) {
	t.Helper()
	storage := newFilesMetaStorage(t)
	selected := seedDeletionData(t, storage, "data", "base")
	seedDeletionData(t, storage, "data:stream:$DATA", "stream")
	wrapped := &failedCreateCleanupStorage{Storage: storage}
	options := testOptions(t)
	options.Storage = wrapped
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	peer := newDeletionPeer(t, server)
	id := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	request := deletionRequest(server, peer)
	open, status := request.Opens.Find(state.FileID{Persistent: id.Persistent, Volatile: id.Volatile}, request.Binding())
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	ctx := t.Context()
	var want error
	switch outcome {
	case "close failure":
		wrapped.closeErr, want = smb.ErrIO, smb.ErrIO
	case "remove failure":
		wrapped.removeErr, want = smb.ErrIO, smb.ErrIO
	case "changed inode":
		replacement := seedDeletionData(t, storage, "replacement", "new base")
		seedDeletionData(t, storage, "replacement:stream:$DATA", "new stream")
		if renameErr := storage.Rename(ctx, smb.RenameRequest{Source: replacement.Name, SourceInode: replacement.Object.Inode, Destination: selected.Name, DestinationInode: selected.Object.Inode, Replace: true}); renameErr != nil {
			t.Fatal(renameErr)
		}
		want = smb.ErrIdentityChanged
	case "remove cancelled":
		wrapped.removeErr, want = context.Canceled, context.Canceled
	}
	// CREATE still owns the parent when response encoding fails. Exercise its
	// direct action consumer, not the bulk cleanup path that reacquires guards.
	unlock, err := lockParent(t.Context(), request, selected.Name.Parent)
	if err != nil {
		t.Fatal(err)
	}
	err = closeFailedCreate(ctx, request, open)
	unlock()
	if want == nil && err != nil || want != nil && !errors.Is(err, want) {
		t.Fatalf("failed CREATE cleanup = %v, want %v", err, want)
	}
	wrapped.closeErr = nil
	switch outcome {
	case "success", "close failure":
		requireDeletionMissing(t, storage, "data:stream:$DATA")
		requireDeletionData(t, storage, "data", "base")
	case "changed inode":
		requireDeletionData(t, storage, "data", "new base")
		requireDeletionData(t, storage, "data:stream:$DATA", "new stream")
	default:
		requireDeletionData(t, storage, "data", "base")
		requireDeletionData(t, storage, "data:stream:$DATA", "stream")
	}
	reservation, status := request.Opens.Reserve(state.OpenRequest{Object: open.Object, Binding: request.Binding(), GrantedAccess: fileDelete, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatalf("failed CREATE retained delete-pending: %#x", status)
	}
	if status := request.Opens.Abort(reservation); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	id = peer.open(t, "data:stream:$DATA", fileReadData, fileOpenIf, 0, smb.StatusSuccess)
	peer.close(t, id)
}

func deletionRequest(server *Server, peer *deletionPeer) RequestContext {
	return RequestContext{
		server: server, Storage: server.options.Storage, Opens: server.options.State,
		Session: Session{SessionID: peer.session.SessionID}, Tree: Tree{TreeID: peer.session.TreeID},
	}
}

func TestDeletionWithActuallyCancelledContext(t *testing.T) {
	for _, boundary := range []string{"bulk cleanup", "cleanup action"} {
		t.Run(boundary, func(t *testing.T) {
			server := deletionServer(t)
			storage := server.options.Storage
			seedDeletionData(t, storage, "data", "base")
			seedDeletionData(t, storage, "data:stream:$DATA", "stream")
			peer := newDeletionPeer(t, server)
			id := peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
			request := deletionRequest(server, peer)
			action, status := request.Opens.Close(state.FileID{Persistent: id.Persistent, Volatile: id.Volatile}, request.Binding())
			if status != smb.StatusSuccess || !action.Remove {
				t.Fatalf("close = %+v, %#x", action, status)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if boundary == "bulk cleanup" {
				if err := server.cleanup(ctx, []state.CloseAction{action}); err != nil {
					t.Fatal(err)
				}
				requireDeletionMissing(t, storage, "data:stream:$DATA")
			} else {
				if err := server.cleanupAction(ctx, action); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled action = %v", err)
				}
				requireDeletionData(t, storage, "data:stream:$DATA", "stream")
			}
			requireDeletionData(t, storage, "data", "base")
			id = peer.open(t, "data:stream:$DATA", fileReadData, fileOpenIf, 0, smb.StatusSuccess)
			peer.close(t, id)
		})
	}
}
