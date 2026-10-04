package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func requireBreakChange(t *testing.T, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-changed:
	default:
		t.Fatal("break state change did not wake waiters")
	}
}

func TestBreakChangesBroadcast(t *testing.T) {
	for _, finish := range []string{"ack", "expire", "close"} {
		t.Run(finish, func(t *testing.T) {
			table, now := clockTable(t)
			req := request(1)
			open := commit(t, table, req, leaseGrant(req, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite))
			changed := table.BreakChanges()
			notification := startBreak(t, table, req.Object, smb.LeaseRead)
			requireBreakChange(t, changed)
			if !table.BreakPending(notification) || !table.LeasesBreaking(req.Object, state.GUID{9}, state.GUID{9}) {
				t.Fatal("break is not pending")
			}
			if table.LeasesBreaking(req.Object, req.ClientGUID, open.LeaseKey) {
				t.Fatal("requesting lease was not excluded")
			}
			changed = table.BreakChanges()
			_, status := table.AckBreak(binding, req.ClientGUID, open.LeaseKey, smb.LeaseWrite)
			statusIs(t, status, smb.StatusRequestNotAccepted)
			select {
			case <-changed:
				t.Fatal("rejected ack woke waiters")
			default:
			}
			switch finish {
			case "ack":
				_, status = table.AckBreak(binding, req.ClientGUID, open.LeaseKey, smb.LeaseRead)
				statusIs(t, status, smb.StatusSuccess)
			case "expire":
				*now = now.Add(state.LeaseBreakTimeout)
				table.ExpireBreaks()
			case "close":
				_, status = table.Close(open.ID, binding)
				statusIs(t, status, smb.StatusSuccess)
			}
			requireBreakChange(t, changed)
			if table.BreakPending(notification) || table.LeasesBreaking(req.Object, state.GUID{9}, state.GUID{9}) {
				t.Fatal("finished break remains pending")
			}
		})
	}
}

func TestClosingAttachedSharedLeaseMemberBroadcasts(t *testing.T) {
	for _, closeBy := range []string{"close", "session", "tree"} {
		t.Run(closeBy, func(t *testing.T) {
			table, _ := clockTable(t)
			req := durableRequest(1, 2)
			grant := durableGrant(req)
			grant.Lease.State |= smb.LeaseWrite
			attached := commit(t, table, req, grant)
			other := req
			other.Binding.SessionID = 2
			other.CreateGUID = state.GUID{4}
			detached := commit(t, table, other, grant)
			if actions := table.Disconnect(2); len(actions) != 0 {
				t.Fatal("durable member closed on disconnect")
			}
			notification := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
			changed := table.BreakChanges()
			var actions []state.CloseAction
			switch closeBy {
			case "close":
				action, status := table.Close(attached.ID, attached.Binding)
				statusIs(t, status, smb.StatusSuccess)
				actions = []state.CloseAction{action}
			case "session":
				actions = table.CloseSession(attached.Binding.SessionID)
			case "tree":
				actions = table.CloseTree(attached.Binding)
			}
			if len(actions) != 1 || actions[0].FileID != attached.ID {
				t.Fatalf("attached close: %+v", actions)
			}
			requireBreakChange(t, changed)
			if !table.BreakPending(notification) {
				t.Fatal("close removed the shared lease before detached completion")
			}
			actions = table.CompleteDetachedBreak(notification)
			if len(actions) != 1 || actions[0].FileID != detached.ID || table.BreakPending(notification) {
				t.Fatalf("detached completion: %+v", actions)
			}
		})
	}
}

func TestAckBreakWithSessionBindingAfterReconnect(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	table.Disconnect(binding.SessionID)
	fresh, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	for _, test := range []struct {
		binding state.Binding
		client  state.GUID
	}{
		{binding: state.Binding{SessionID: binding.SessionID}, client: req.ClientGUID},
		{binding: state.Binding{SessionID: fresh.Binding.SessionID, TreeID: fresh.Binding.TreeID + 1}, client: req.ClientGUID},
		{binding: state.Binding{SessionID: fresh.Binding.SessionID}, client: state.GUID{9}},
	} {
		_, status = table.AckBreak(test.binding, test.client, open.LeaseKey, smb.LeaseRead)
		if status == smb.StatusSuccess {
			t.Fatal("wrong identity acknowledged break")
		}
	}
	_, status = table.AckBreak(state.Binding{SessionID: fresh.Binding.SessionID}, req.ClientGUID, open.LeaseKey, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
}

func TestCompleteDetachedBreak(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	table.Disconnect(binding.SessionID)
	notification := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	changed := table.BreakChanges()
	actions := table.CompleteDetachedBreak(notification)
	if len(actions) != 1 || actions[0].Handle != open.Handle || table.BreakPending(notification) {
		t.Fatalf("detached break completion: %+v", actions)
	}
	requireBreakChange(t, changed)
}

func TestCompleteDetachedBreaksScopesPendingCleanup(t *testing.T) {
	table, _ := clockTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	otherReq := durableRequest(2, 4)
	otherGrant := durableGrant(otherReq)
	otherGrant.Lease.State |= smb.LeaseWrite
	commit(t, table, otherReq, otherGrant)
	attachedReq := durableRequest(3, 6)
	attachedReq.Binding.SessionID = 2
	attachedGrant := durableGrant(attachedReq)
	attachedGrant.Lease.State |= smb.LeaseWrite
	commit(t, table, attachedReq, attachedGrant)
	first := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	other := startBreak(t, table, otherReq.Object, smb.LeaseRead|smb.LeaseHandle)
	attached := startBreak(t, table, attachedReq.Object, smb.LeaseRead|smb.LeaseHandle)
	if actions := table.Disconnect(binding.SessionID); len(actions) != 0 {
		t.Fatal("durable members closed before detached completion")
	}
	if notifications, actions := table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, smb.LeaseRead|smb.LeaseHandle); len(notifications) != 0 || len(actions) != 0 {
		t.Fatal("an earlier pending break emitted duplicate work")
	}
	if actions := table.CompleteDetachedBreaks(req.Object, req.ClientGUID, open.LeaseKey); len(actions) != 0 || !table.BreakPending(first) {
		t.Fatal("completion revoked the requesting lease")
	}
	actions := table.CompleteDetachedBreaks(req.Object, state.GUID{9}, state.GUID{9})
	if len(actions) != 1 || actions[0].FileID != open.ID || table.BreakPending(first) {
		t.Fatalf("earlier pending completion: %+v", actions)
	}
	if !table.BreakPending(other) || !table.BreakPending(attached) {
		t.Fatal("completion changed another object's break")
	}
	if actions := table.CompleteDetachedBreaks(attachedReq.Object, state.GUID{9}, state.GUID{9}); len(actions) != 0 || !table.BreakPending(attached) {
		t.Fatal("completion revoked an attached lease")
	}
	if actions := table.CompleteDetachedBreaks(req.Object, state.GUID{9}, state.GUID{9}); len(actions) != 0 {
		t.Fatal("completion returned duplicate cleanup")
	}
}

func TestCompleteDetachedBreakDoesNotRevokeReattachedLease(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	table.Disconnect(binding.SessionID)
	notification := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	_, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	if actions := table.CompleteDetachedBreak(notification); len(actions) != 0 || !table.BreakPending(notification) {
		t.Fatal("completion revoked a reattached lease")
	}
}
