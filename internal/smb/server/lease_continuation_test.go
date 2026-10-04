package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestQueuedLeaseBreakCancellationPreservesCapturedStage(t *testing.T) {
	for _, cancelQueued := range []bool{false, true} {
		t.Run(map[bool]string{false: "original waiter", true: "queued waiter"}[cancelQueued], func(t *testing.T) {
			checkQueuedLeaseBreakCancellation(t, cancelQueued)
		})
	}
}

func checkQueuedLeaseBreakCancellation(t *testing.T, cancelQueued bool) {
	t.Helper()
	server, holder, _ := newCreateLeaseClients(t)
	created := holder.create(t, leaseCreateRequest("cancel-queued-break"), leaseV2(1, 7))
	open, status := server.options.State.Find(state.FileID(created.Reply.ID), state.Binding{SessionID: holder.session.SessionID, TreeID: holder.session.TreeID})
	if status != smb.StatusSuccess {
		t.Fatalf("created lease open: %#x", status)
	}
	originalCtx, cancelOriginal := context.WithCancel(holder.ctx)
	defer cancelOriginal()
	originalDone := startServerBreak(originalCtx, server, open, 3)
	first, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil {
		t.Fatal(err)
	}
	captured, exists := server.options.State.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists {
		t.Fatal("captured lease missing")
	}
	changed := server.options.State.BreakChanges()
	queuedCtx, cancelStronger := context.WithCancel(holder.ctx)
	defer cancelStronger()
	queuedDone := startServerBreak(queuedCtx, server, open, 0)
	waitAsyncSignal(holder.ctx, t, changed)
	canceled, survivor, cancel := originalDone, queuedDone, cancelOriginal
	if cancelQueued {
		canceled, survivor, cancel = queuedDone, originalDone, cancelStronger
	}
	cancel()
	if waitErr := waitAsyncResult(holder.ctx, t, canceled); !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("canceled waiter: %v", waitErr)
	}
	current, exists := server.options.State.LeaseFor(open.Object, open.ClientGUID, open.LeaseKey)
	if !exists || current.State != captured.State || current.BreakTo != captured.BreakTo || current.Epoch != captured.Epoch || current.Deadline != captured.Deadline || current.EffectiveState() != 0 {
		t.Fatalf("cancellation changed captured stage or discarded queued revocation: %+v", current)
	}
	holder.ack(t, first)
	second, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil || second.CurrentState != 3 || second.NewState != 1 || second.Epoch != first.Epoch || second.Flags != 1 {
		t.Fatalf("continuation after cancellation: %+v, error %v", second, err)
	}
	select {
	case breakErr := <-survivor:
		t.Fatalf("surviving waiter completed before the next ACK: %v", breakErr)
	default:
	}
	holder.ack(t, second)
	last, err := holder.client.WaitLeaseBreak(holder.ctx)
	if err != nil || last.CurrentState != 1 || last.NewState != 0 || last.Epoch != first.Epoch || last.Flags != 0 {
		t.Fatalf("final continuation after cancellation: %+v, error %v", last, err)
	}
	finishServerBreak(holder.ctx, t, survivor)
	holder.close(t, wire.FileID(open.ID))
}

func TestQueuedLeaseBreakCloseCompletesBothWaiters(t *testing.T) {
	server, holder, _ := newCreateLeaseClients(t)
	created := holder.create(t, leaseCreateRequest("close-queued-break"), leaseV2(1, 7))
	binding := state.Binding{SessionID: holder.session.SessionID, TreeID: holder.session.TreeID}
	open, status := server.options.State.Find(state.FileID(created.Reply.ID), binding)
	if status != smb.StatusSuccess {
		t.Fatalf("created lease open: %#x", status)
	}
	path, pathErr := server.options.Storage.PathOf(holder.ctx, open.Object.Inode)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	selected, lookupErr := server.options.Storage.Lookup(holder.ctx, path)
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	if !selected.Exists || selected.Object != open.Object {
		t.Fatalf("incoherent CLOSE fixture: open %+v, selected %+v", open.Object, selected.Object)
	}
	originalDone := startServerBreak(holder.ctx, server, open, 3)
	if _, err := holder.client.WaitLeaseBreak(holder.ctx); err != nil {
		t.Fatal(err)
	}
	changed := server.options.State.BreakChanges()
	queuedDone := startServerBreak(holder.ctx, server, open, 0)
	waitAsyncSignal(holder.ctx, t, changed)
	holder.close(t, wire.FileID(open.ID))
	finishServerBreak(holder.ctx, t, originalDone)
	finishServerBreak(holder.ctx, t, queuedDone)
	_, status = server.options.State.Find(open.ID, binding)
	if status != smb.StatusFileClosed || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("CLOSE kept the open or queued revocation")
	}
}

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
