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
