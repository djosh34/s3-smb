package server

import (
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
