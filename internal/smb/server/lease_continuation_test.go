package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestQueuedLeaseBreakCreatesUseSameEpochContinuations(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES128GCM} {
		t.Run(fmt.Sprintf("cipher_%d", cipher), func(t *testing.T) {
			checkQueuedLeaseBreakCreates(t, cipher)
		})
	}
}

func checkQueuedLeaseBreakCreates(t *testing.T, cipher uint16) {
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
	holder := loginCreateLeaseClientWithCipher(t, server, 2, cipher)
	reader := loginCreateLeaseClientWithCipher(t, server, 3, cipher)
	writer := loginCreateLeaseClientWithCipher(t, server, 4, cipher)
	request := leaseCreateRequest("queued-break")
	opened := holder.create(t, request, leaseV2(1, 7))
	assertLeaseGrant(t, opened, 7)
	readID := reader.send(t, request, nil)
	first, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.CurrentState != 7 || first.NewState != 3 || first.Flags != 1 || first.Key != opened.Lease.Key || first.Epoch != opened.Lease.Epoch+1 {
		t.Fatalf("initial RWH-to-RH notification: %+v", first)
	}
	readDone := receiveCreateLater(reader, readID)
	assertCreateWaits(t, readDone)
	changed := server.options.State.BreakChanges()
	overwrite := request
	overwrite.DesiredAccess = fileWriteData
	overwrite.Disposition = fileOverwrite
	writeID := writer.send(t, overwrite, nil)
	writeDone := receiveCreateLater(writer, writeID)
	select {
	case <-changed:
	case <-writer.ctx.Done():
		t.Fatal(writer.ctx.Err())
	}
	assertCreateWaits(t, writeDone)
	joined := holder.create(t, request, leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != 7 || joined.Lease.Epoch != first.Epoch || joined.Lease.Flags != leaseParentKeySet|leaseBreakInProgress {
		t.Fatalf("same-key CREATE during queued break: %+v", joined.Lease)
	}
	holder.close(t, joined.Reply.ID)
	holder.ack(t, first)
	second, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.LeaseBreakNotification{Key: first.Key, Epoch: first.Epoch, Flags: 1, CurrentState: 3, NewState: 1}
	if second != want {
		t.Fatalf("RH-to-R continuation: %+v, want %+v", second, want)
	}
	assertCreateWaits(t, readDone)
	assertCreateWaits(t, writeDone)
	joined = holder.create(t, request, leaseV2(1, 7))
	if joined.Lease == nil || joined.Lease.State != 3 || joined.Lease.Epoch != first.Epoch || joined.Lease.Flags != leaseParentKeySet|leaseBreakInProgress {
		t.Fatalf("same-key CREATE during RH-to-R stage: %+v", joined.Lease)
	}
	holder.close(t, joined.Reply.ID)
	holder.ack(t, second)
	last, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	want.Flags, want.CurrentState, want.NewState = 0, 1, 0
	if last != want {
		t.Fatalf("R-only no-ACK final notification: %+v, want %+v", last, want)
	}
	readResult := finishCreate(t, readDone)
	writeResult := finishCreate(t, writeDone)
	if readResult.Reply.Action != 1 || writeResult.Reply.Action != 3 {
		t.Fatalf("queued CREATE results: reader %+v, writer %+v", readResult.Reply, writeResult.Reply)
	}
	reader.close(t, readResult.Reply.ID)
	writer.close(t, writeResult.Reply.ID)
	holder.close(t, opened.Reply.ID)
}
