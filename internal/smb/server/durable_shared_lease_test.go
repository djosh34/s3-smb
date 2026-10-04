package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func createSharedDurable(t *testing.T, client *createLeaseClient, lease *wire.LeaseContext) smbtest.CreateResult {
	t.Helper()
	header := client.header(wire.Create)
	if err := client.client.SendCreate(client.ctx, header, smbtest.CreateOptions{
		Request: leaseCreateRequest("file"), Lease: lease, Durable: &wire.DurableRequest{CreateGUID: [16]byte{5}},
	}); err != nil {
		t.Fatal(err)
	}
	return client.created(t, header.MessageID)
}

func TestDurableJoinGrantsOnExistingSharedH(t *testing.T) {
	server, holder, writer := newCreateLeaseClients(t)
	first := holder.create(t, leaseCreateRequest("file"), leaseV2(1, smb.LeaseRead|smb.LeaseHandle))
	request := leaseCreateRequest("file")
	request.DesiredAccess = fileWriteData
	id := writer.send(t, request, nil)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	holder.ack(t, notification)
	written := writer.created(t, id)
	joined := createSharedDurable(t, holder, leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != smb.LeaseHandle || joined.Durable == nil || joined.Durable.Timeout != 120000 {
		t.Fatalf("existing H durable join = %+v", joined)
	}
	open := durableOpen(t, server, holder.session, joined.Reply.ID)
	if !open.Durable {
		t.Fatal("existing H join was not retained as durable")
	}
	writer.close(t, written.Reply.ID)
	holder.close(t, first.Reply.ID)
	holder.close(t, joined.Reply.ID)
}

func TestDurableJoinRefusesPendingNoneEvenWhenReplyHasH(t *testing.T) {
	server, holder, writer := newCreateLeaseClients(t)
	first := holder.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	request := leaseCreateRequest("file")
	request.Disposition, request.DesiredAccess = fileOverwrite, fileWriteData
	id := writer.send(t, request, nil)
	notification, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.NewState != 0 {
		t.Fatalf("overwrite target = %#x", notification.NewState)
	}
	joined := createSharedDurable(t, holder, leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State&smb.LeaseHandle == 0 || joined.Lease.Flags&leaseBreakInProgress == 0 || joined.Durable != nil {
		t.Fatalf("pending NONE durable join = %+v", joined)
	}
	open := durableOpen(t, server, holder.session, joined.Reply.ID)
	if open.Durable {
		t.Fatal("pending H-removing break published durability")
	}
	holder.close(t, first.Reply.ID)
	holder.ack(t, notification)
	writerOpen := writer.created(t, id)
	writer.close(t, writerOpen.Reply.ID)
	holder.close(t, joined.Reply.ID)
}
