package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// CLOSE resolves PathOf once before table removal and again during cleanup.
// Pause the second result before cleanup can acquire its parent guard.
type pausedDeletionPathStorage struct {
	smb.Storage
	entered   chan struct{}
	resume    chan struct{}
	remaining atomic.Int32
}

func (storage *pausedDeletionPathStorage) PathOf(ctx context.Context, inode smb.Inode) (string, error) {
	path, err := storage.Storage.PathOf(ctx, inode)
	if storage.remaining.Add(-1) == 0 {
		close(storage.entered)
		select {
		case <-storage.resume:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return path, err
}

func TestDeleteOnCloseRetriesRenameBetweenPathAndGuard(t *testing.T) {
	for _, stream := range []string{"", ":stream:$DATA"} {
		for _, outcome := range []string{"renamed", "replaced", "gone"} {
			t.Run(stream+"/"+outcome, func(t *testing.T) {
				checkDeletionPathRace(t, stream, outcome)
			})
		}
	}
}

func checkDeletionPathRace(t *testing.T, stream, outcome string) {
	t.Helper()
	storage := newFilesMetaStorage(t)
	source := seedDeletionData(t, storage, "selected", "old base")
	if stream != "" {
		seedDeletionData(t, storage, "selected"+stream, "old stream")
		seedDeletionData(t, storage, "selected:other:$DATA", "keep stream")
	}
	paused := &pausedDeletionPathStorage{Storage: storage, entered: make(chan struct{}), resume: make(chan struct{})}
	var once sync.Once
	resume := func() { once.Do(func() { close(paused.resume) }) }
	defer resume()
	options := testOptions(t)
	options.Storage = paused
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	peer := newDeletionPeer(t, server)
	var held wire.FileID
	if stream != "" {
		held = peer.open(t, "selected", fileReadData, fileOpen, 0, smb.StatusSuccess)
	}
	id := peer.open(t, "selected"+stream, fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	paused.remaining.Store(2)
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := peer.client.Send(peer.ctx, []wire.Message{peer.message(wire.Close, body)}); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case <-paused.entered:
	case <-peer.ctx.Done():
		t.Fatal(peer.ctx.Err())
	}
	if outcome == "gone" {
		if removeErr := storage.Remove(t.Context(), source.Name, source.Object.Inode); removeErr != nil {
			t.Fatal(removeErr)
		}
	} else {
		destination, lookupErr := storage.Lookup(t.Context(), "moved")
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		if renameErr := storage.Rename(t.Context(), smb.RenameRequest{Source: source.Name, SourceInode: source.Object.Inode, Destination: destination.Name}); renameErr != nil {
			t.Fatal(renameErr)
		}
	}
	if outcome != "renamed" {
		seedDeletionData(t, storage, "selected", "replacement base")
		if stream != "" {
			seedDeletionData(t, storage, "selected"+stream, "replacement stream")
		}
	}
	resume()
	response, err := peer.client.Receive(peer.ctx)
	if err != nil || response.Messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE after %s = %+v, %v", outcome, response, err)
	}
	if outcome != "gone" {
		requireDeletionMissing(t, storage, "moved"+stream)
		if stream != "" {
			requireDeletionData(t, storage, "moved", "old base")
			requireDeletionData(t, storage, "moved:other:$DATA", "keep stream")
		}
	}
	if outcome != "renamed" {
		requireDeletionData(t, storage, "selected", "replacement base")
		if stream != "" {
			requireDeletionData(t, storage, "selected"+stream, "replacement stream")
		}
	}
	if held != (wire.FileID{}) && outcome != "gone" {
		peer.close(t, held)
	}
}
