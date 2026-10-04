package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestEarlierLeaseBreakCompletesAfterOriginalWaiterCancels(t *testing.T) {
	server, storage := fixedClockLeaseServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
	firstCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	first := startServerBreak(firstCtx, server, open, smb.LeaseRead|smb.LeaseHandle)
	if _, err := client.WaitLeaseBreak(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := waitAsyncResult(ctx, t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("original waiter: %v", err)
	}
	secondCtx := &observedBreakWait{Context: ctx, waiting: make(chan struct{}), after: 1}
	second := startServerBreak(secondCtx, server, open, smb.LeaseRead|smb.LeaseHandle)
	waitAsyncSignal(ctx, t, secondCtx.waiting)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	finishServerBreak(ctx, t, second)
	if storage.closed.Load() != 1 || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("earlier detached break kept rights or its handle")
	}
}

func TestSharedLeaseBreakCompletesAfterLastAttachedMemberCloses(t *testing.T) {
	for _, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
		t.Run(map[wire.Command]string{wire.Logoff: "logoff", wire.TreeDisconnect: "tree disconnect"}[command], func(t *testing.T) {
			server, storage := fixedClockLeaseServer(t)
			client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
			_, _, other := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
			open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
			member := joinDurableLease(t, server, other, open)
			if actions := server.options.State.Disconnect(other.SessionID); len(actions) != 0 {
				t.Fatal("durable member closed on disconnect")
			}
			waitCtx := &observedBreakWait{Context: ctx, waiting: make(chan struct{}), after: 2}
			done := startServerBreak(waitCtx, server, open, smb.LeaseRead|smb.LeaseHandle)
			if _, err := client.WaitLeaseBreak(ctx); err != nil {
				t.Fatal(err)
			}
			waitAsyncSignal(ctx, t, waitCtx.waiting)
			response := exchange(ctx, t, client, treeRequest(t, session, session.NextMessageID, command))[0]
			if response.Header.Status != smb.StatusSuccess {
				t.Fatal(response.Header)
			}
			finishServerBreak(ctx, t, done)
			if storage.closed.Load() != 2 || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
				t.Fatal("shared detached break kept rights or a handle")
			}
			if _, status := server.options.State.Reconnect(state.ReconnectRequest{ID: member.ID, Binding: member.Binding, User: member.User, Share: member.Share, ClientGUID: member.ClientGUID, CreateGUID: member.CreateGUID, LeaseKey: member.LeaseKey}); status != smb.StatusObjectNameNotFound {
				t.Fatalf("detached member survived completion: %#x", status)
			}
		})
	}
}

func TestLeaseBreakBlockedSendCompletesAfterSessionReplacement(t *testing.T) {
	for _, cleanupError := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cleanup error"}[cleanupError], func(t *testing.T) {
			checkBlockedLeaseBreakReplacement(t, cleanupError)
		})
	}
}
