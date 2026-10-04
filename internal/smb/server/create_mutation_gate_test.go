package server

import (
	"context"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// This storage-boundary barrier does not know about the state mutation API.
// It keeps actual WriteAt in flight without holding an adapter inode lock.
// All successful bytes still go through the real file/SQLite adapter.
type createMutationWriteStorage struct {
	smb.Storage
	writeErr error
	entered  chan struct{}
	release  chan struct{}
}

func (storage *createMutationWriteStorage) WriteAt(ctx context.Context, handle smb.Handle, data []byte, offset uint64) (int, error) {
	close(storage.entered)
	select {
	case <-storage.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if storage.writeErr != nil {
		return 0, storage.writeErr
	}
	return storage.Storage.WriteAt(ctx, handle, data, offset)
}

func TestCreateDuringWriteWithholdsFreshReadLease(t *testing.T) {
	for _, outcome := range []string{"success", "error", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			checkCreateDuringWriteWithholdsFreshReadLease(t, outcome)
		})
	}
}

func checkCreateDuringWriteWithholdsFreshReadLease(t *testing.T, outcome string) {
	t.Helper()
	options := testOptions(t)
	storage := &createMutationWriteStorage{Storage: newFilesMetaStorage(t), entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(storage.release) })
	t.Cleanup(unblock)
	if outcome == "error" {
		storage.writeErr = smb.ErrIO
	}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	holder, writer, reader := loginCreateLeaseClient(t, server, 2), loginCreateLeaseClient(t, server, 3), loginCreateLeaseClient(t, server, 4)
	holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
	ordinary := leaseCreateRequest("file")
	ordinary.Disposition, ordinary.DesiredAccess = fileOpen, fileWriteData
	opened := writer.create(t, ordinary, nil)
	shared := holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
	assertLeaseGrant(t, shared, smb.LeaseRead)
	if shared.Lease.Epoch != 8 || shared.Lease.Flags != leaseParentKeySet {
		t.Fatalf("ordinary writer OPEN changed R: %+v", shared.Lease)
	}

	// Drain a real R-only invalidation if the companion hook is present. With
	// the pre-gate production checkpoint no notification arrives; cancellation
	// at cleanup joins this receiver without introducing a fake state helper.
	notifications := make(chan error, 1)
	notifyCtx, stopNotifications := context.WithCancel(holder.ctx)
	go func() {
		_, receiveErr := holder.client.WaitLeaseBreak(notifyCtx)
		notifications <- receiveErr
	}()
	t.Cleanup(func() { stopNotifications(); <-notifications })

	write := writer.header(wire.Write)
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: opened.Reply.ID, Data: []byte("new")})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := writer.client.Send(writer.ctx, []wire.Message{{Header: write, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	select {
	case <-storage.entered:
	case <-writer.ctx.Done():
		t.Fatal(writer.ctx.Err())
	}
	pending, err := writer.client.Receive(writer.ctx)
	if err != nil || len(pending.Messages) != 1 {
		t.Fatalf("WRITE pending = %+v, error %v", pending, err)
	}
	header := pending.Messages[0].Header
	if header.MessageID != write.MessageID || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 {
		t.Fatalf("WRITE pending identity = %+v", header)
	}

	during := reader.create(t, leaseCreateRequest("file"), leaseV2(2, smb.LeaseRead))
	if during.Lease == nil || during.Lease.State != 0 || during.Reply.OplockLevel != 0 {
		// Continue through every release outcome even on the expected RED, so
		// the failing grant oracle never leaves the blocked adapter behind.
		t.Errorf("fresh R granted during actual WriteAt: lease %+v, oplock %#x", during.Lease, during.Reply.OplockLevel)
	}
	final := finishCreateMutationWrite(t, writer, write, header, unblock, outcome)
	want := smb.StatusSuccess
	switch outcome {
	case "error":
		want = smb.StatusIODeviceError
	case "cancel":
		want = smb.StatusCancelled
	}
	if final.Header.Status != want || final.Header.MessageID != write.MessageID || final.Header.Flags&wire.FlagAsync == 0 || final.Header.AsyncID != header.AsyncID || final.Header.Credit != 0 {
		t.Fatalf("WRITE final identity/status = %+v, want %#x", final.Header, want)
	}
	if outcome == "success" {
		written, decodeErr := wire.DecodeWriteResponse(final)
		if decodeErr != nil || written.Count != 3 {
			t.Fatalf("WRITE result = %+v, error %v", written, decodeErr)
		}
	}
	after := reader.create(t, leaseCreateRequest("file"), leaseV2(3, smb.LeaseRead))
	assertLeaseGrant(t, after, smb.LeaseRead)
	if after.Lease.Epoch != 8 || after.Lease.Flags != leaseParentKeySet {
		t.Fatalf("released mutation still withheld R: %+v", after.Lease)
	}
	checkCreateMutationWriteBytes(t, reader, after.Reply.ID, outcome)
	t.Logf("%s release completed; final async identity, restored R and actual bytes verified", outcome)
	reader.close(t, during.Reply.ID)
	reader.close(t, after.Reply.ID)
	writer.close(t, opened.Reply.ID)
}

func finishCreateMutationWrite(t *testing.T, writer *createLeaseClient, write, pending wire.Header, unblock func(), outcome string) wire.Message {
	t.Helper()
	if outcome == "cancel" {
		cancel := writer.header(wire.Cancel)
		cancel.MessageID, cancel.AsyncID = write.MessageID, pending.AsyncID
		cancel.Credit, cancel.CreditCharge, cancel.Flags = 0, 0, wire.FlagAsync
		cancel.TreeID = 0
		body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if sendErr := writer.client.Send(writer.ctx, []wire.Message{{Header: cancel, Body: body}}); sendErr != nil {
			t.Fatal(sendErr)
		}
	} else {
		unblock()
	}
	final, err := writer.receive(write.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	return final
}

func checkCreateMutationWriteBytes(t *testing.T, reader *createLeaseClient, id wire.FileID, outcome string) {
	t.Helper()
	header := reader.header(wire.Read)
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := reader.client.Send(reader.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	result, err := reader.receive(header.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "success" {
		if result.Header.Status != smb.StatusEndOfFile {
			t.Fatalf("failed/canceled WRITE changed bytes: %#x", result.Header.Status)
		}
		return
	}
	read, decodeErr := wire.DecodeReadResponse(result)
	if result.Header.Status != smb.StatusSuccess || decodeErr != nil || string(read.Data) != "new" {
		t.Fatalf("successful WRITE bytes = %+v, status %#x, error %v", read, result.Header.Status, decodeErr)
	}
}
