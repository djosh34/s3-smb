package server

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCreateLeaseSameTransportAllowsAcknowledgmentAndClose(t *testing.T) {
	for _, closeHolder := range []bool{false, true} {
		for _, cipher := range []uint16{0, smb.CipherAES128GCM} {
			t.Run(fmt.Sprintf("close-%v-cipher-%d", closeHolder, cipher), func(t *testing.T) {
				checkCreateLeaseSameTransport(t, closeHolder, cipher)
			})
		}
	}
}

func checkCreateLeaseSameTransport(t *testing.T, closeHolder bool, cipher uint16) {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client := loginCreateLeaseClientWithCipher(t, server, 2, cipher)
	held := leaseCreateRequest("file")
	requested := uint32(7)
	if closeHolder {
		held.ShareAccess, requested = 1, 3
	}
	opened := client.create(t, held, leaseV2(1, requested))
	other := leaseCreateRequest("file")
	if closeHolder {
		other.DesiredAccess = fileWriteData
	}
	id := client.send(t, other, leaseV2(2, 7))
	pending, err := client.client.Receive(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Messages) != 1 {
		t.Fatalf("pending replies = %+v", pending.Messages)
	}
	header := pending.Messages[0].Header
	if header.MessageID != id || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 {
		t.Fatalf("pending CREATE = %+v", header)
	}
	notification, err := client.client.WaitLeaseBreak(client.ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := uint32(3)
	if closeHolder {
		target = 0
	}
	if notification.CurrentState != requested || notification.NewState != target || notification.Key != [16]byte{1} {
		t.Fatalf("same-transport break = %+v", notification)
	}
	if closeHolder {
		client.close(t, opened.Reply.ID)
	} else {
		client.ack(t, notification)
	}
	message, err := client.receive(id)
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.AsyncID != header.AsyncID || message.Header.Flags&wire.FlagAsync == 0 || message.Header.Credit != 0 {
		t.Fatalf("final CREATE = %+v", message.Header)
	}
	result, err := wire.DecodeCreateResponse(message)
	if err != nil || message.Header.Status != smb.StatusSuccess || result.ID == (wire.FileID{}) {
		t.Fatalf("CREATE after holder progress = %+v, error %v", message.Header, err)
	}
	client.close(t, result.ID)
}

func TestCreateLeaseTimeoutCompletesWaitingOpen(t *testing.T) {
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	base := time.Now()
	var elapsed atomic.Int64
	options.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	table, err := state.New(options.Now)
	if err != nil {
		t.Fatal(err)
	}
	options.State = table
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	first, second := loginCreateLeaseClient(t, server, 2), loginCreateLeaseClient(t, server, 3)
	opened := first.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	id := second.send(t, leaseCreateRequest("file"), leaseV2(2, 7))
	notification, err := first.client.WaitLeaseBreak(first.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification.NewState != 3 {
		t.Fatalf("timeout break = %+v", notification)
	}
	done := receiveCreateLater(second, id)
	assertCreateWaits(t, done)
	elapsed.Store(int64(state.LeaseBreakTimeout))
	if actions := table.ExpireBreaks(); len(actions) != 0 {
		t.Fatalf("ordinary lease expiry returned cleanup: %+v", actions)
	}
	result := finishCreate(t, done)
	assertLeaseGrant(t, result, 3)
	open, status := table.Find(state.FileID(opened.Reply.ID), state.Binding{SessionID: first.session.SessionID, TreeID: first.session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatalf("timed-out holder = %#x", status)
	}
	lease, exists := table.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists || lease.State != 0 || lease.Breaking {
		t.Fatalf("timed-out lease = %+v", lease)
	}
}

func TestCanceledCreateLeaseWaitDoesNotMutateOrCommit(t *testing.T) {
	server, first, second := newCreateLeaseClients(t)
	opened := first.create(t, leaseCreateRequest("file"), leaseV2(1, 7))
	open, status := server.options.State.Find(state.FileID(opened.Reply.ID), state.Binding{SessionID: first.session.SessionID, TreeID: first.session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatalf("holder = %#x", status)
	}
	size := uint64(19)
	if err := server.options.Storage.SetAttr(first.ctx, open.Object, smb.AttrChange{Size: &size}); err != nil {
		t.Fatal(err)
	}
	other := leaseCreateRequest("file")
	other.Disposition, other.DesiredAccess = fileOverwrite, fileWriteData
	id := second.send(t, other, nil)
	notification, err := first.client.WaitLeaseBreak(first.ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := second.client.Receive(second.ctx)
	if err != nil || len(pending.Messages) != 1 || pending.Messages[0].Header.Status != smb.StatusPending {
		t.Fatalf("pending overwrite = %+v, error %v", pending, err)
	}
	cancel := second.header(wire.Cancel)
	cancel.MessageID = id
	cancel.Credit, cancel.CreditCharge = 0, 0
	cancel.TreeID, cancel.AsyncID = 0, pending.Messages[0].Header.AsyncID
	cancel.Flags = wire.FlagAsync
	body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := second.client.Send(second.ctx, []wire.Message{{Header: cancel, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	message, err := second.receive(id)
	if err != nil || message.Header.Status != smb.StatusCancelled {
		t.Fatalf("canceled overwrite = %+v, error %v", message.Header, err)
	}
	attr, err := server.options.Storage.GetAttr(first.ctx, open.Object)
	if err != nil || attr.Size != size {
		t.Fatalf("canceled overwrite changed data: size %d, error %v", attr.Size, err)
	}
	// A canceled CREATE left its reservation behind if deny-all sharing fails.
	first.ack(t, notification)
	denyAll := leaseCreateRequest("file")
	denyAll.DesiredAccess, denyAll.ShareAccess = 0x80, 1
	first.create(t, denyAll, nil)
}
