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
	for _, stream := range []bool{false, true} {
		name := "base"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			storage := &pausedAllocationStorage{
				Storage: newFilesMetaStorage(t), entered: make(chan struct{}), resume: make(chan struct{}),
			}
			allocator := newReadWriteClient(t, storage)
			client, ctx, session := newFilesMetaClient(t, allocator.server)
			other := &readWriteClient{client: client, server: allocator.server, ctx: ctx, session: session, next: session.NextMessageID}
			// Release before either connection's cleanup, even on assertion failure.
			t.Cleanup(storage.unblock)
			base := createdFile(t, allocator.create(t, createRequest("data", fileCreateDisposition)))
			selected := base
			path := "data"
			if stream {
				path += ":fork"
				selected = createdFile(t, allocator.create(t, createRequest(path, fileCreateDisposition)))
			}
			competing := createdFile(t, other.create(t, createRequest(path, fileOpen)))
			requireIOStatus(t, allocator.write(t, wire.WriteRequest{ID: selected.ID, Data: bytes.Repeat([]byte("a"), 9000)}, 1), smb.StatusSuccess)
			allocation, err := wire.EncodeFileAllocationInformation(wire.FileAllocationInformation{AllocationSize: 4097})
			if err != nil {
				t.Fatal(err)
			}
			body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: selected.ID, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileAllocation), Input: allocation})
			if err != nil {
				t.Fatal(err)
			}
			request := ioMessage(allocator.session, allocator.next, wire.SetInfo, body, 1)
			allocator.next++
			storage.armed.Store(true)
			if err := allocator.client.Send(allocator.ctx, []wire.Message{request}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-storage.entered:
			case <-allocator.ctx.Done():
				t.Fatal(allocator.ctx.Err())
			}
			eof, err := wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: 7})
			if err != nil {
				t.Fatal(err)
			}
			body, err = wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: competing.ID, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileEndOfFile), Input: eof})
			if err != nil {
				t.Fatal(err)
			}
			requireIOStatus(t, other.exchange(t, wire.SetInfo, body, 1), smb.StatusSuccess)
			storage.unblock()
			response, err := allocator.client.Receive(allocator.ctx)
			if err != nil || len(response.Messages) != 1 {
				t.Fatalf("allocation reply: %+v, %v", response.Messages, err)
			}
			if response.Messages[0].Header.Status == smb.StatusPending {
				pending := response.Messages[0]
				response, err = allocator.client.Receive(allocator.ctx)
				if err != nil || len(response.Messages) != 1 {
					t.Fatalf("allocation final: %+v, %v", response.Messages, err)
				}
				assertAsyncFinal(t, response.Messages[0], pending, smb.StatusSuccess)
			}
			final := response.Messages[0]
			if final.Header.MessageID != request.Header.MessageID || final.Header.Command != wire.SetInfo || final.Header.SessionID != allocator.session.SessionID {
				t.Fatalf("allocation identity: %+v", final.Header)
			}
			requireIOStatus(t, final, smb.StatusSuccess)
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
			if stream {
				baseAttr, err := storage.Lookup(t.Context(), "data")
				if err != nil || baseAttr.Attr.Size != 0 {
					t.Fatalf("stream changed base: %+v, %v", baseAttr, err)
				}
			}
		})
	}
}
