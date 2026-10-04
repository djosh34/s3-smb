package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateLeaseReopenReportsHeldHandleOnlyState(t *testing.T) {
	_, holder, opener := newCreateLeaseClients(t)
	opened := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 3))
	request := leaseCreateRequest("file")
	request.DesiredAccess = fileWriteData
	id := opener.send(t, request, nil)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	holder.ack(t, notification)
	writer := opener.created(t, id)
	joined := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	assertLeaseGrant(t, joined, smb.LeaseHandle)
	if joined.Lease.Epoch != notification.Epoch || joined.Lease.ParentKey != [16]byte{8} || joined.Lease.Flags != leaseParentKeySet {
		t.Fatalf("held H response = %+v", joined.Lease)
	}
	opener.close(t, writer.Reply.ID)
	// Once the other writer closes, a supported RWH promotion is safe again.
	promoted := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	assertLeaseGrant(t, promoted, 7)
	if promoted.Lease.Epoch != notification.Epoch+1 {
		t.Fatalf("H promotion epoch = %d", promoted.Lease.Epoch)
	}
	holder.close(t, opened.Reply.ID)
	holder.close(t, joined.Reply.ID)
	holder.close(t, promoted.Reply.ID)
}

func TestCreateLeaseReopenJoinsPendingBreakToNone(t *testing.T) {
	_, holder, opener := newCreateLeaseClients(t)
	opened := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	request := leaseCreateRequest("file")
	request.Disposition, request.DesiredAccess = fileOverwrite, fileWriteData
	id := opener.send(t, request, nil)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.NewState != 0 {
		t.Fatalf("overwrite break = %+v", notification)
	}
	joined := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != 7 || joined.Lease.Epoch != notification.Epoch || joined.Lease.Flags != leaseParentKeySet|leaseBreakInProgress {
		t.Fatalf("shared NONE-target response = %+v", joined.Lease)
	}
	// The new member must retain the lease while the original handle closes.
	holder.close(t, opened.Reply.ID)
	done := receiveCreateLater(opener, id)
	assertCreateWaits(t, done)
	holder.ack(t, notification)
	finishCreate(t, done)
	holder.close(t, joined.Reply.ID)
}
