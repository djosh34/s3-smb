package server

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Pause one mutation before entering the real adapter's inode coordinator.
type pausedAllocationStorage struct {
	smb.Storage
	entered chan struct{}
	resume  chan struct{}
	armed   atomic.Bool
	once    sync.Once
}

func (s *pausedAllocationStorage) SetAttr(ctx context.Context, object smb.ObjectKey, change smb.AttrChange) error {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Storage.SetAttr(ctx, object, change)
}

func (s *pausedAllocationStorage) unblock() {
	s.once.Do(func() { close(s.resume) })
}

func TestAllocationCannotUndoConcurrentEOFShrink(t *testing.T) {
	for _, path := range []string{"data", "data:fork"} {
		t.Run(path, func(t *testing.T) { checkAllocationAfterEOFShrink(t, path) })
	}
}

func checkAllocationAfterEOFShrink(t *testing.T, path string) {
	t.Helper()
	storage := &pausedAllocationStorage{
		Storage: newFilesMetaStorage(t), entered: make(chan struct{}), resume: make(chan struct{}),
	}
	allocator := newReadWriteClient(t, storage)
	client, ctx, session := newFilesMetaClient(t, allocator.server)
	other := &readWriteClient{client: client, server: allocator.server, ctx: ctx, session: session, next: session.NextMessageID}
	// Release before either connection's cleanup, even on assertion failure.
	t.Cleanup(storage.unblock)
	selected := createdFile(t, allocator.create(t, createRequest("data", fileCreateDisposition)))
	if path != "data" {
		selected = createdFile(t, allocator.create(t, createRequest(path, fileCreateDisposition)))
	}
	competing := createdFile(t, other.create(t, createRequest(path, fileOpen)))
	requireIOStatus(t, allocator.write(t, wire.WriteRequest{ID: selected.ID, Data: bytes.Repeat([]byte("a"), 9000)}, 1), smb.StatusSuccess)
	request := allocationRaceSizeRequest(t, allocator, selected.ID, wire.ClassFileAllocation, 4097)
	storage.armed.Store(true)
	if err := allocator.client.Send(allocator.ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-storage.entered:
	case <-allocator.ctx.Done():
		t.Fatal(allocator.ctx.Err())
	}
	eof := allocationRaceSizeRequest(t, other, competing.ID, wire.ClassFileEndOfFile, 7)
	requireIOStatus(t, ioRoundTrip(other.ctx, t, other.client, eof), smb.StatusSuccess)
	storage.unblock()
	finishPausedAllocation(t, allocator, request)
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	attr, err := storage.GetAttr(t.Context(), resolved.Object)
	if err != nil || attr.Size != 7 {
		t.Fatalf("allocation re-extended EOF: size %d, want 7; error %v", attr.Size, err)
	}
	data, err := wire.DecodeReadResponse(other.read(t, wire.ReadRequest{ID: competing.ID, Length: 9000}, 1))
	if err != nil || string(data.Data) != "aaaaaaa" {
		t.Fatalf("retained bytes: %q, %v", data.Data, err)
	}
	if path != "data" {
		baseAttr, lookupErr := storage.Lookup(t.Context(), "data")
		if lookupErr != nil || baseAttr.Attr.Size != 0 {
			t.Fatalf("stream changed base: %+v, %v", baseAttr, lookupErr)
		}
	}
}

func allocationRaceSizeRequest(t *testing.T, client *readWriteClient, id wire.FileID, class wire.FileInfoClass, size uint64) wire.Message {
	t.Helper()
	var input []byte
	var err error
	if class == wire.ClassFileAllocation {
		input, err = wire.EncodeFileAllocationInformation(wire.FileAllocationInformation{AllocationSize: size})
	} else {
		input, err = wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: size})
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(client.session, client.next, wire.SetInfo, body, 1)
	client.next++
	return message
}

func finishPausedAllocation(t *testing.T, client *readWriteClient, request wire.Message) {
	t.Helper()
	response, err := client.client.Receive(client.ctx)
	if err != nil || len(response.Messages) != 1 {
		t.Fatalf("allocation reply: %+v, %v", response.Messages, err)
	}
	if response.Messages[0].Header.Status == smb.StatusPending {
		pending := response.Messages[0]
		response, err = client.client.Receive(client.ctx)
		if err != nil || len(response.Messages) != 1 {
			t.Fatalf("allocation final: %+v, %v", response.Messages, err)
		}
		assertAsyncFinal(t, response.Messages[0], pending, smb.StatusSuccess)
	}
	final := response.Messages[0]
	if final.Header.MessageID != request.Header.MessageID || final.Header.Command != wire.SetInfo || final.Header.SessionID != client.session.SessionID {
		t.Fatalf("allocation identity: %+v", final.Header)
	}
	requireIOStatus(t, final, smb.StatusSuccess)
}
