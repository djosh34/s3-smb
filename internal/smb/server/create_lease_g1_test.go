package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestOrdinaryWriterOpenPreservesReadLease(t *testing.T) {
	_, holder, writer := newCreateLeaseClients(t)
	initial := holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
	request := leaseCreateRequest("file")
	request.Disposition, request.DesiredAccess = fileOpen, fileWriteData
	opened := writer.create(t, request, leaseV2(2, smb.LeaseRead))
	assertLeaseGrant(t, opened, smb.LeaseRead)
	if opened.Lease.Epoch != 8 {
		t.Fatalf("competing R acquisition epoch = %d, want 8", opened.Lease.Epoch)
	}
	shared := holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
	assertLeaseGrant(t, shared, smb.LeaseRead)
	if shared.Lease.Epoch != 8 || shared.Lease.Flags != leaseParentKeySet {
		t.Fatalf("ordinary writer OPEN broke R: %+v", shared.Lease)
	}
	holder.close(t, initial.Reply.ID)
	holder.close(t, shared.Reply.ID)
	writer.close(t, opened.Reply.ID)
}

func TestCreateSameKeyPendingReportsHeldLease(t *testing.T) {
	_, holder, other := newCreateLeaseClients(t)
	initial := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	id := other.send(t, leaseCreateRequest("file"), nil)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.CurrentState != 7 || notification.NewState != smb.LeaseRead|smb.LeaseHandle || notification.Epoch != 9 {
		t.Fatalf("captured break = %+v", notification)
	}
	done := receiveCreateLater(other, id)
	assertCreateWaits(t, done)
	for _, requested := range []uint32{0, smb.LeaseRead, 7} {
		wanted := leaseV2(1, requested)
		wanted.Epoch, wanted.ParentKey = 999, [16]byte{9}
		reopened := holder.create(t, leaseCreateRequest("file"), wanted)
		assertLeaseGrant(t, reopened, 7)
		if reopened.Lease.Epoch != 9 || reopened.Lease.Flags != leaseParentKeySet|leaseBreakInProgress || reopened.Lease.ParentKey != [16]byte{8} {
			t.Fatalf("pending response changed shared lease: %+v", reopened.Lease)
		}
		holder.close(t, reopened.Reply.ID)
	}
	holder.ack(t, notification)
	opened := finishCreate(t, done)
	holder.close(t, initial.Reply.ID)
	other.close(t, opened.Reply.ID)
}

func TestMetadataOnlyCreateDoesNotBreakWriteCaching(t *testing.T) {
	_, holder, metadata := newCreateLeaseClients(t)
	initial := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	request := leaseCreateRequest("file")
	request.Disposition, request.DesiredAccess = fileOpen, 0x00120180
	id := metadata.send(t, request, nil)
	done := receiveCreateLater(metadata, id)
	select {
	case result := <-done:
		if result.err != nil || result.message.Header.Status != smb.StatusSuccess {
			t.Fatalf("metadata OPEN = %#x, error %v", result.message.Header.Status, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("metadata-only OPEN waited for an unrelated lease break")
	}
	shared := holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead))
	assertLeaseGrant(t, shared, 7)
	if shared.Lease.Epoch != 8 || shared.Lease.Flags != leaseParentKeySet {
		t.Fatalf("metadata OPEN changed held lease: %+v", shared.Lease)
	}
	holder.close(t, initial.Reply.ID)
	holder.close(t, shared.Reply.ID)
}

func TestOrdinaryWritableOpenBreaksOnlyWriteCaching(t *testing.T) {
	for _, access := range []uint32{fileWriteData, fileAppendData} {
		t.Run(fmt.Sprintf("access-%x", access), func(t *testing.T) {
			_, holder, writer := newCreateLeaseClients(t)
			initial := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
			ordinary := leaseCreateRequest("file")
			ordinary.Disposition, ordinary.DesiredAccess = fileOpen, access
			id := writer.send(t, ordinary, leaseV2(2, 7))
			notification, err := holder.client.WaitLeaseBreak(holder.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if notification.CurrentState != 7 || notification.NewState != smb.LeaseRead|smb.LeaseHandle || notification.Epoch != 9 || notification.Flags != 1 {
				t.Fatalf("ordinary OPEN break = %+v, want RWH to RH at epoch 9", notification)
			}
			holder.ack(t, notification)
			opened := writer.created(t, id)
			assertLeaseGrant(t, opened, smb.LeaseRead|smb.LeaseHandle)
			if opened.Lease.Epoch != 8 {
				t.Fatalf("writer acquisition epoch = %d, want 8", opened.Lease.Epoch)
			}
			holder.close(t, initial.Reply.ID)
			writer.close(t, opened.Reply.ID)
		})
	}
}

func TestCreateSameKeySmallerRequestReturnsSharedLease(t *testing.T) {
	for _, requested := range []uint32{0, smb.LeaseRead, smb.LeaseHandle} {
		t.Run(fmt.Sprintf("requested-%x", requested), func(t *testing.T) {
			_, client, _ := newCreateLeaseClients(t)
			initial := client.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead|smb.LeaseHandle))
			wanted := leaseV2(1, requested)
			wanted.Epoch, wanted.ParentKey = 999, [16]byte{9}
			reopened := client.create(t, leaseCreateRequest("file"), wanted)
			assertLeaseGrant(t, reopened, smb.LeaseRead|smb.LeaseHandle)
			if reopened.Lease.Epoch != 8 || reopened.Lease.ParentKey != [16]byte{8} || reopened.Lease.Flags != leaseParentKeySet {
				t.Fatalf("reopen changed shared fields: %+v", reopened.Lease)
			}
			client.close(t, initial.Reply.ID)
			client.close(t, reopened.Reply.ID)
		})
	}
}
