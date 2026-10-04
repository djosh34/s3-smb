package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCreateBreaksEveryOtherLeaseKey(t *testing.T) {
	_, holder, opener := newCreateLeaseClients(t)
	holder.create(t, leaseCreateRequest("file"), leaseV2(1, 3))
	holder.create(t, leaseCreateRequest("file"), leaseV2(2, 3))
	request := leaseCreateRequest("file")
	request.DesiredAccess = fileWriteData
	id := opener.send(t, request, nil)
	first, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := receiveCreateLater(opener, id)
	holder.ack(t, first)
	assertCreateWaits(t, done)
	second, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Key == second.Key || first.CurrentState != 3 || second.CurrentState != 3 ||
		first.NewState != smb.LeaseHandle || second.NewState != smb.LeaseHandle {
		t.Fatalf("object breaks = %+v, %+v", first, second)
	}
	holder.ack(t, second)
	finishCreate(t, done)
}
