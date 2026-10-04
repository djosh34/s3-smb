package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCreateDirectoryCollisionPrecedesKindMismatch(t *testing.T) {
	storage := newFilesMetaStorage(t)
	seedDeletionData(t, storage, "regular", "preserved")
	client := newReadWriteClient(t, storage)
	for _, disposition := range []uint32{fileCreateDisposition, fileOpen, fileOpenIf} {
		request := createRequest("regular", disposition)
		request.Options = fileDirectoryFile
		want := smb.StatusNotADirectory
		if disposition == fileCreateDisposition {
			want = smb.StatusObjectNameCollision
		}
		requireIOStatus(t, client.create(t, request), want)
		requireDeletionData(t, storage, "regular", "preserved")
	}
	requireIOStatus(t, ioRoundTrip(client.ctx, t, client.client, sessionEcho(t, client.session, client.next)), smb.StatusSuccess)
}

func TestCreateReserveOpfilterRefusedBeforeMutation(t *testing.T) {
	storage := newFilesMetaStorage(t)
	seedDeletionData(t, storage, "existing", "preserved")
	client := newReadWriteClient(t, storage)
	for _, name := range []string{"absent", "existing"} {
		request := createRequest(name, fileSupersede)
		request.Options, request.ShareAccess = 0x00100000, 0
		requireIOStatus(t, client.create(t, request), smb.StatusNotSupported)
		selected, err := storage.Lookup(t.Context(), name)
		if err != nil || selected.Exists != (name == "existing") {
			t.Fatalf("refused OPFILTER mutated namespace: %+v, %v", selected, err)
		}
		if selected.Exists {
			requireDeletionData(t, storage, name, "preserved")
		}
		// A refused share-zero open must not retain its sharing reservation.
		opened := createdFile(t, client.create(t, createRequest(name, fileOpenIf)))
		requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
	}
	requireIOStatus(t, ioRoundTrip(client.ctx, t, client.client, sessionEcho(t, client.session, client.next)), smb.StatusSuccess)
}

func TestCreateDestructiveHiddenSystemRequiresRequestedBits(t *testing.T) {
	storage := newFilesMetaStorage(t)
	client := newReadWriteClient(t, storage)
	for _, disposition := range []uint32{fileSupersede, fileOverwrite, fileOverwriteIf} {
		for _, attributes := range []uint32{0x2, 0x4, 0x6} {
			t.Run(fmt.Sprintf("disposition_%d_attributes_%x", disposition, attributes), func(t *testing.T) {
				name := fmt.Sprintf("protected-%d-%x", disposition, attributes)
				request := createRequest(name, fileCreateDisposition)
				request.FileAttributes = attributes
				opened := createdFile(t, client.create(t, request))
				writeCreatedFile(t, client, opened.ID, "preserved")
				requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
				for _, desired := range []uint32{0, attributes & 0x2, attributes & 0x4} {
					if desired == attributes {
						continue
					}
					request.Disposition, request.FileAttributes = disposition, desired
					requireIOStatus(t, client.create(t, request), smb.StatusAccessDenied)
					requireDeletionData(t, storage, name, "preserved")
					selected, err := storage.Lookup(t.Context(), name)
					if err != nil || selected.Attr.Attributes != attributes|0x20 {
						t.Fatalf("refused CREATE changed attributes: %+v, %v", selected, err)
					}
				}
				request.FileAttributes = attributes
				replaced := createdFile(t, client.create(t, request))
				readCreatedFile(t, client, replaced.ID, "")
				requireIOStatus(t, client.close(t, replaced.ID, 0), smb.StatusSuccess)
			})
		}
	}
	requireIOStatus(t, ioRoundTrip(client.ctx, t, client.client, sessionEcho(t, client.session, client.next)), smb.StatusSuccess)
}

func TestCreateInvalidImpersonationPreservesExistingData(t *testing.T) {
	storage := newFilesMetaStorage(t)
	seedDeletionData(t, storage, "impersonation-data", "preserved")
	client := newReadWriteClient(t, storage)
	for _, disposition := range []uint32{fileSupersede, fileOverwrite, fileOverwriteIf} {
		request := createRequest("impersonation-data", disposition)
		request.ImpersonationLevel = 4
		requireIOStatus(t, client.create(t, request), smb.StatusBadImpersonationLevel)
		requireDeletionData(t, storage, "impersonation-data", "preserved")
	}
	requireIOStatus(t, ioRoundTrip(client.ctx, t, client.client, sessionEcho(t, client.session, client.next)), smb.StatusSuccess)
}

func TestCreateWriteThroughOrdinaryWriteWaitsForBarrier(t *testing.T) {
	for _, failBarrier := range []bool{false, true} {
		t.Run(fmt.Sprintf("barrier_failure_%t", failBarrier), func(t *testing.T) {
			checkCreateWriteThrough(t, failBarrier)
		})
	}
}

func checkCreateWriteThrough(t *testing.T, failBarrier bool) {
	t.Helper()
	barrier := &writeBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	var fail atomic.Bool
	fail.Store(failBarrier)
	fixture := newIOFixture(t, flushBarrier(func(ctx context.Context, full bool) error {
		if err := barrier.Commit(ctx, full); err != nil {
			return err
		}
		if fail.Swap(false) {
			return smb.ErrIO
		}
		return nil
	}))
	client := newReadWriteClient(t, fixture.adapter)
	unblock := sync.OnceFunc(func() { close(barrier.resume) })
	t.Cleanup(unblock)
	request := createRequest("flush-data", fileCreateDisposition)
	request.Options = 0x00000002 // FILE_WRITE_THROUGH in CREATE, not WRITE.
	opened := createdFile(t, client.create(t, request))
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: opened.ID, Data: []byte("durable"), Flags: 0})
	if err != nil {
		t.Fatal(err)
	}
	message := ioMessage(client.session, client.next, wire.Write, body, 1)
	client.next++
	if sendErr := client.client.Send(client.ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	pending, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	requireIOStatus(t, pending.Messages[0], smb.StatusPending)
	select {
	case <-barrier.entered:
	case <-client.ctx.Done():
		t.Fatal(client.ctx.Err())
	}
	if barrier.full || fixture.store.puts.Load() == 0 {
		t.Fatal("CREATE write-through did not upload with SyncData")
	}
	assertCommittedFlushData(t, fixture, []byte("durable"))
	// ECHO is next while WRITE is still blocked in the metadata barrier.
	requireIOStatus(t, ioRoundTrip(client.ctx, t, client.client, sessionEcho(t, client.session, client.next)), smb.StatusSuccess)
	client.next++
	unblock()
	final, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := smb.StatusSuccess
	if failBarrier {
		want = smb.StatusIODeviceError
	}
	assertAsyncFinal(t, final.Messages[0], pending.Messages[0], want)
	if !failBarrier {
		written, err := wire.DecodeWriteResponse(final.Messages[0])
		if err != nil || written.Count != 7 {
			t.Fatalf("write-through final count: %+v, %v", written, err)
		}
	}
	requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
}
