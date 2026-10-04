package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type deletionPeer struct {
	ctx     context.Context
	client  *smbtest.Client
	session smbtest.Session
	next    uint64
}

func newDeletionPeer(t *testing.T, server *Server) *deletionPeer {
	t.Helper()
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	return &deletionPeer{ctx: ctx, client: client, session: session, next: session.NextMessageID}
}

func (peer *deletionPeer) message(command wire.Command, body []byte) wire.Message {
	message := wire.Message{Header: wire.Header{Command: command, MessageID: peer.next, SessionID: peer.session.SessionID, TreeID: peer.session.TreeID, CreditCharge: 1, Credit: 16}, Body: body}
	peer.next++
	return message
}

func (peer *deletionPeer) open(t *testing.T, path string, access, disposition, options uint32, want smb.Status) wire.FileID {
	t.Helper()
	body, err := wire.EncodeCreateRequest(wire.CreateRequest{Name: path, DesiredAccess: access, Disposition: disposition, Options: options, ShareAccess: 7, ImpersonationLevel: 2})
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(peer.ctx, t, peer.client, peer.message(wire.Create, body))
	if response.Header.Status != want {
		t.Fatalf("CREATE %s: %#x, want %#x", path, response.Header.Status, want)
	}
	if want != smb.StatusSuccess {
		return wire.FileID{}
	}
	create, err := wire.DecodeCreateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return create.ID
}

func (peer *deletionPeer) close(t *testing.T, id wire.FileID) {
	t.Helper()
	peer.closeExpect(t, id, smb.StatusSuccess)
}

func (peer *deletionPeer) closeExpect(t *testing.T, id wire.FileID, want smb.Status) {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	response := exchange(peer.ctx, t, peer.client, peer.message(wire.Close, body))[0]
	if response.Header.Status != want {
		t.Fatalf("CLOSE: %#x, want %#x", response.Header.Status, want)
	}
}

func (peer *deletionPeer) disposition(t *testing.T, id wire.FileID) {
	t.Helper()
	input, err := wire.EncodeFileDispositionInformation(wire.FileDispositionInformation{DeletePending: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileDisposition), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(peer.ctx, t, peer.client, peer.message(wire.SetInfo, body))
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("SET_INFO disposition: %#x", response.Header.Status)
	}
}

func deletionServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func seedDeletionData(t *testing.T, storage smb.Storage, path, data string) smb.Resolved {
	t.Helper()
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = storage.Create(t.Context(), resolved.Name, smb.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := storage.WriteAt(t.Context(), handle, []byte(data), 0)
	closeErr := storage.Close(t.Context(), handle)
	if err := errors.Join(writeErr, closeErr); err != nil || n != len(data) {
		t.Fatalf("seed %s = %d, %v", path, n, err)
	}
	return resolved
}

func requireDeletionData(t *testing.T, storage smb.Storage, path, want string) {
	t.Helper()
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil || !resolved.Exists {
		t.Fatalf("lookup %s = %+v, %v", path, resolved, err)
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len(want)+1)
	n, readErr := storage.ReadAt(t.Context(), handle, data, 0)
	closeErr := storage.Close(t.Context(), handle)
	if closeErr != nil || !errors.Is(readErr, io.EOF) || !bytes.Equal(data[:n], []byte(want)) {
		t.Fatalf("read %s = %q, %v, close %v; want %q", path, data[:n], readErr, closeErr, want)
	}
}

func requireDeletionMissing(t *testing.T, storage smb.Storage, path string) {
	t.Helper()
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil || resolved.Exists {
		t.Fatalf("deleted %s = %+v, %v", path, resolved, err)
	}
}

func TestStreamDeleteOnClosePreservesBaseAndOtherStreams(t *testing.T) {
	for _, ending := range []string{"close", "drop", "logoff", "tree disconnect", "shutdown"} {
		for _, other := range []string{"none", "base", "same stream"} {
			t.Run(ending+"/"+other, func(t *testing.T) { checkStreamDeletion(t, ending, other) })
		}
	}
}

func checkStreamDeletion(t *testing.T, ending, other string) {
	t.Helper()
	server := deletionServer(t)
	storage := server.options.Storage
	seedDeletionData(t, storage, "data", "keep base data")
	seedDeletionData(t, storage, "data:AFP_Resource:$DATA", "delete resource")
	seedDeletionData(t, storage, "data:keep:$DATA", "keep other stream")
	peer := newDeletionPeer(t, server)
	var held wire.FileID
	if other != "none" {
		path := "data"
		if other == "same stream" {
			path = "data:AFP_Resource:$DATA"
		}
		held = peer.open(t, path, fileReadData, fileOpen, 0, smb.StatusSuccess)
	}
	deleted := peer.open(t, "data:AFP_Resource:$DATA", fileReadData|fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
	if ending == "close" {
		peer.close(t, deleted)
		if other == "same stream" {
			requireDeletionData(t, storage, "data:AFP_Resource:$DATA", "delete resource")
			peer.open(t, "data:AFP_Resource:$DATA", fileReadData, fileOpen, 0, smb.StatusDeletePending)
			base := peer.open(t, "data", fileReadData, fileOpen, 0, smb.StatusSuccess)
			peer.close(t, base)
		}
		if held != (wire.FileID{}) {
			if other == "base" {
				requireDeletionMissing(t, storage, "data:AFP_Resource:$DATA")
			}
			peer.close(t, held)
		}
	} else {
		endDeletionSession(t, server, peer, ending)
	}
	requireDeletionMissing(t, storage, "data:AFP_Resource:$DATA")
	requireDeletionData(t, storage, "data", "keep base data")
	requireDeletionData(t, storage, "data:keep:$DATA", "keep other stream")
}

func endDeletionSession(t *testing.T, server *Server, peer *deletionPeer, ending string) {
	t.Helper()
	switch ending {
	case "drop":
		if err := peer.client.Close(); err != nil {
			t.Fatal(err)
		}
		server.workers.Wait()
	case "shutdown":
		if err := server.Shutdown(peer.ctx); err != nil {
			t.Fatal(err)
		}
	case "logoff", "tree disconnect":
		command := wire.Logoff
		if ending == "tree disconnect" {
			command = wire.TreeDisconnect
		}
		message := treeRequest(t, peer.session, peer.next, command)
		peer.next++
		response := exchange(peer.ctx, t, peer.client, message)[0]
		if response.Header.Status != smb.StatusSuccess {
			t.Fatalf("%s: %#x", ending, response.Header.Status)
		}
	default:
		t.Fatalf("unknown ending %q", ending)
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
	peer.disposition(t, id)
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

// Pausing Close exposes the bulk-close window after Table.CloseSession has
// removed the last open but before cleanup takes the deletion's parent guard.
type pausedDeletionStorage struct {
	smb.Storage
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (storage *pausedDeletionStorage) Close(ctx context.Context, handle smb.Handle) error {
	storage.once.Do(func() {
		close(storage.entered)
		<-storage.resume
	})
	return storage.Storage.Close(ctx, handle)
}

func TestLogoffDeletionRacesCreateWithoutRemovingNewOpen(t *testing.T) {
	for range 64 {
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
}
