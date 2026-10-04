package server

import (
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestStreamDeleteOnClosePreservesBaseAndOtherStreams(t *testing.T) {
	for _, ending := range []string{"close", "drop", "logoff", "tree disconnect", "shutdown"} {
		for _, other := range []string{"none", "base", "same stream"} {
			t.Run(ending+"/"+other, func(t *testing.T) { checkStreamDeletion(t, ending, other) })
		}
	}
}

func TestDeleteOnCloseDeniedBeforeStorageMutation(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "existing", "keep data")
	seedDeletionData(t, storage, "existing:stream:$DATA", "keep stream")
	peer := newDeletionPeer(t, server)
	peer.open(t, "missing", fileWriteData, fileCreateDisposition, fileDeleteOnClose, smb.StatusAccessDenied)
	requireDeletionMissing(t, storage, "missing")
	peer.open(t, "existing", fileWriteData, fileOverwrite, fileDeleteOnClose, smb.StatusAccessDenied)
	peer.open(t, "existing:stream:$DATA", fileWriteData, fileOverwrite, fileDeleteOnClose, smb.StatusAccessDenied)
	requireDeletionData(t, storage, "existing", "keep data")
	requireDeletionData(t, storage, "existing:stream:$DATA", "keep stream")
	id := peer.open(t, "missing", 0x10000000, fileCreateDisposition, fileDeleteOnClose, smb.StatusSuccess)
	peer.close(t, id)
	requireDeletionMissing(t, storage, "missing")
}

func TestDispositionDeletePendingRejectsNewOpens(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "base")
	seedDeletionData(t, storage, "data:stream:$DATA", "stream")
	peer := newDeletionPeer(t, server)
	id := peer.open(t, "data", fileDelete, fileOpen, 0, smb.StatusSuccess)
	held := peer.open(t, "data", fileReadData, fileOpen, 0, smb.StatusSuccess)
	peer.disposition(t, id, true, smb.StatusSuccess)
	peer.open(t, "data", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	peer.open(t, "data:stream:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	peer.close(t, id)
	requireDeletionData(t, storage, "data", "base")
	peer.open(t, "data", fileReadData, fileOpen, 0, smb.StatusDeletePending)
	peer.close(t, held)
	requireDeletionMissing(t, storage, "data")
}

func TestBaseDeleteOnClosePreservesUnrelatedName(t *testing.T) {
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "selected", "delete this")
	seedDeletionData(t, storage, "unrelated", "keep this")
	peer := newDeletionPeer(t, server)
	id := peer.open(t, "selected", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	other := peer.open(t, "unrelated", fileReadData, fileOpen, 0, smb.StatusSuccess)
	peer.close(t, id)
	peer.close(t, other)
	requireDeletionMissing(t, storage, "selected")
	requireDeletionData(t, storage, "unrelated", "keep this")
}

func TestDeleteOnCloseFollowsRenameAndPreservesReplacement(t *testing.T) {
	for _, stream := range []string{"", ":stream:$DATA"} {
		t.Run(stream, func(t *testing.T) {
			server := deletionServer(t)
			storage := server.options.Storage
			source := seedDeletionData(t, storage, "selected", "base data")
			if stream != "" {
				seedDeletionData(t, storage, "selected"+stream, "delete stream")
				seedDeletionData(t, storage, "selected:other:$DATA", "other stream")
			}
			peer := newDeletionPeer(t, server)
			id := peer.open(t, "selected"+stream, fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
			destination, err := storage.Lookup(t.Context(), "renamed")
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Rename(t.Context(), smb.RenameRequest{Source: source.Name, SourceInode: source.Object.Inode, Destination: destination.Name}); err != nil {
				t.Fatal(err)
			}
			seedDeletionData(t, storage, "selected", "replacement data")
			peer.close(t, id)
			requireDeletionMissing(t, storage, "renamed"+stream)
			requireDeletionData(t, storage, "selected", "replacement data")
			if stream != "" {
				requireDeletionData(t, storage, "renamed", "base data")
				requireDeletionData(t, storage, "renamed:other:$DATA", "other stream")
			}
		})
	}
}

func TestLogoffDeletionRacesCreateWithoutRemovingNewOpen(t *testing.T) {
	// The close barrier forces the deletion-pending window deterministically.
	// Use go test -count for stress instead of allocating 64 identical runtimes.
	t.Run("race", func(t *testing.T) {
		storage := newFilesMetaStorage(t)
		seedDeletionData(t, storage, "data", "old data")
		paused := &pausedDeletionStorage{Storage: storage, entered: make(chan struct{}), resume: make(chan struct{})}
		var resume sync.Once
		unblock := func() { resume.Do(func() { close(paused.resume) }) }
		defer unblock()
		options := testOptions(t)
		options.Storage = paused
		server, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		deleting, creating := newDeletionPeer(t, server), newDeletionPeer(t, server)
		deleting.open(t, "data", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
		message := treeRequest(t, deleting.session, deleting.next, wire.Logoff)
		if sendErr := deleting.client.Send(deleting.ctx, []wire.Message{message}); sendErr != nil {
			t.Fatal(sendErr)
		}
		select {
		case <-paused.entered:
		case <-deleting.ctx.Done():
			t.Fatal(deleting.ctx.Err())
		}
		creating.open(t, "data", fileReadData, fileOpenIf, 0, smb.StatusDeletePending)
		unblock()
		response, err := deleting.client.Receive(deleting.ctx)
		if err != nil || response.Messages[0].Header.Status != smb.StatusSuccess {
			t.Fatalf("LOGOFF = %+v, %v", response, err)
		}
		id := creating.open(t, "data", fileReadData, fileOpenIf, 0, smb.StatusSuccess)
		creating.close(t, id)
		resolved, err := storage.Lookup(t.Context(), "data")
		if err != nil || !resolved.Exists {
			t.Fatalf("new CREATE lost its file: %+v, %v", resolved, err)
		}
	})
}
