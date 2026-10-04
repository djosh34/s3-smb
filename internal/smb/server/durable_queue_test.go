package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestDurableJoinRefusesQueuedNoneWithCapturedRH(t *testing.T) {
	server, holder, opener := newCreateLeaseClients(t)
	writer := loginCreateLeaseClient(t, server, 4)
	initial := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	openID := opener.send(t, leaseCreateRequest("file"), nil)
	first, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.CurrentState != 7 || first.NewState != smb.LeaseRead|smb.LeaseHandle || first.Epoch != 9 {
		t.Fatalf("captured break = %+v", first)
	}
	changed := server.options.State.BreakChanges()
	overwrite := leaseCreateRequest("file")
	overwrite.Disposition, overwrite.DesiredAccess = fileOverwrite, fileWriteData
	writeID := writer.send(t, overwrite, nil)
	select {
	case <-changed:
	case <-holder.ctx.Done():
		t.Fatal(holder.ctx.Err())
	}
	joined := createSharedDurable(t, holder, leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != 7 || joined.Lease.Epoch != 9 || joined.Lease.Flags != leaseParentKeySet|leaseBreakInProgress || joined.Durable != nil {
		t.Errorf("queued NONE promised durability or changed captured response: %+v, lease %+v", joined, joined.Lease)
	}
	open := durableOpen(t, server, holder.session, joined.Reply.ID)
	if open.Durable {
		t.Error("atomic Commit retained a new durable grant while ultimate target was NONE")
	}
	holder.ack(t, first)
	second, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.CurrentState != smb.LeaseRead|smb.LeaseHandle || second.NewState != smb.LeaseRead || second.Epoch != 9 || second.Flags != 1 {
		t.Fatalf("queued RH-to-R continuation = %+v", second)
	}
	holder.ack(t, second)
	last, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last.CurrentState != smb.LeaseRead || last.NewState != 0 || last.Epoch != 9 || last.Flags != 0 {
		t.Fatalf("queued R-to-NONE continuation = %+v", last)
	}
	ordinary := opener.created(t, openID)
	mutated := writer.created(t, writeID)
	holder.close(t, initial.Reply.ID)
	holder.close(t, joined.Reply.ID)
	opener.close(t, ordinary.Reply.ID)
	writer.close(t, mutated.Reply.ID)
}
