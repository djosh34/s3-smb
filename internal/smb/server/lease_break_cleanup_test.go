package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func joinDurableLease(t *testing.T, server *Server, session smbtest.Session, open state.Open) {
	t.Helper()
	request := state.OpenRequest{Object: open.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: open.User, Share: open.Share, ClientGUID: open.ClientGUID, CreateGUID: state.GUID{99}, Sharing: 7}
	grant := state.Grant{Handle: cleanupHandle{object: open.Object}, DurableTimeout: smb.DefaultDurableTimeout, Lease: state.Lease{ClientGUID: open.ClientGUID, Key: open.LeaseKey, State: smb.LeaseRead | smb.LeaseHandle | smb.LeaseWrite}}
	commitLeaseOpen(t, server, request, grant)
}

func TestLeaseAcknowledgmentCleansDetachedMembers(t *testing.T) {
	for _, cleanupError := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "storage error"}[cleanupError], func(t *testing.T) {
			server, client, ctx, session, storage := leaseServer(t, smb.CipherAES128GCM, smb.SigningCMAC)
			_, _, other := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
			open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
			joinDurableLease(t, server, other, open)
			server.options.State.Disconnect(other.SessionID)
			done := startServerBreak(ctx, server, open, smb.LeaseRead)
			if _, err := client.WaitLeaseBreak(ctx); err != nil {
				t.Fatal(err)
			}
			if cleanupError {
				storage.closeErr = smb.ErrIO
			}
			response := acknowledgeBreak(ctx, t, client, session, session.NextMessageID, open.LeaseKey, smb.LeaseRead)
			want := smb.StatusSuccess
			if cleanupError {
				want = smb.StatusIODeviceError
			}
			if response.Header.Status != want || storage.closed.Load() != 1 {
				t.Fatalf("ack cleanup: %+v, closed %d", response.Header, storage.closed.Load())
			}
			// The reply proves cleanup finished before changing the injected error.
			storage.closeErr = nil
			finishServerBreak(ctx, t, done)
			found, status := server.options.State.Find(open.ID, open.Binding)
			if status != smb.StatusSuccess || found.Durable {
				t.Fatal("ack failed to remove attached durability")
			}
		})
	}
}

func TestLeaseAcknowledgmentAfterReattachment(t *testing.T) {
	server, client, ctx, session, _ := leaseServer(t, smb.CipherAES128GCM, smb.SigningGMAC)
	otherClient, _, other := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
	breaks, actions := server.options.State.BreakLeases(open.Object, state.GUID{9}, state.GUID{9}, smb.LeaseRead|smb.LeaseHandle)
	if len(breaks) != 1 || len(actions) != 0 {
		t.Fatal("missing notification")
	}
	sent := make(chan error, 1)
	go func() { sent <- server.sendLeaseBreak(ctx, breaks[0]) }()
	if _, err := client.WaitLeaseBreak(ctx); err != nil {
		t.Fatal(err)
	}
	finishServerBreak(ctx, t, sent)
	server.options.State.Disconnect(session.SessionID)
	fresh, status := server.options.State.Reconnect(state.ReconnectRequest{ID: open.ID, Binding: state.Binding{SessionID: other.SessionID, TreeID: other.TreeID}, ClientGUID: open.ClientGUID, LeaseKey: open.LeaseKey, CreateGUID: open.CreateGUID, User: open.User, Share: open.Share})
	if status != smb.StatusSuccess || fresh.ID.Volatile == open.ID.Volatile {
		t.Fatal("reattachment failed")
	}
	response := acknowledgeBreak(ctx, t, client, session, session.NextMessageID, open.LeaseKey, smb.LeaseRead)
	if response.Header.Status != smb.StatusInvalidParameter {
		t.Fatal("old session acknowledged the reattached lease")
	}
	response = acknowledgeBreak(ctx, t, otherClient, other, other.NextMessageID, open.LeaseKey, smb.LeaseRead)
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("new session ack failed: %+v", response.Header)
	}
}

func TestLeaseBreakWaitIncludesEarlierBreakAndExcludesRequester(t *testing.T) {
	server, client, ctx, session, _ := leaseServer(t, smb.CipherAES128GCM, smb.SigningCMAC)
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, false)
	if err := server.BreakLeases(ctx, open.Object, open.ClientGUID, open.LeaseKey, 0); err != nil {
		t.Fatal(err)
	}
	first := startServerBreak(ctx, server, open, smb.LeaseRead)
	if _, err := client.WaitLeaseBreak(ctx); err != nil {
		t.Fatal(err)
	}
	second := startServerBreak(ctx, server, open, smb.LeaseRead)
	select {
	case err := <-second:
		t.Fatalf("second wait ignored the earlier break: %v", err)
	default:
	}
	response := acknowledgeBreak(ctx, t, client, session, session.NextMessageID, open.LeaseKey, smb.LeaseRead)
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	finishServerBreak(ctx, t, first)
	finishServerBreak(ctx, t, second)
}

func TestLeaseBreakCanceledBeforeStartChangesNothing(t *testing.T) {
	server, _, ctx, session, _ := leaseServer(t, smb.CipherAES128GCM, smb.SigningCMAC)
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle, false)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := server.BreakLeases(canceled, open.Object, state.GUID{9}, state.GUID{9}, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled break: %v", err)
	}
	if server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("canceled call started a break")
	}
}
