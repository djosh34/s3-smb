package state_test

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func clockTable(t *testing.T) (*state.Table, *time.Time) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	table, err := state.New(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return table, &now
}

// durableRequest reads inode with CREATE GUID {create}; durableGrant asks for
// an RH lease keyed by that GUID and the default durable timeout.
func durableRequest(inode smb.Inode, create byte) state.OpenRequest {
	req := request(inode)
	req.CreateGUID = state.GUID{create}
	req.GrantedAccess = 0x00120104
	req.SharingIntent = state.RightRead
	return req
}

func durableGrant(req state.OpenRequest) state.Grant {
	return state.Grant{
		DurableTimeout: smb.DefaultDurableTimeout,
		Lease:          state.Lease{ClientGUID: req.ClientGUID, Key: req.CreateGUID, State: smb.LeaseRead | smb.LeaseHandle},
	}
}

func reconnectRequest(open state.Open) state.ReconnectRequest {
	return state.ReconnectRequest{
		ID: open.ID, User: open.User, Share: open.Share, ClientGUID: open.ClientGUID,
		CreateGUID: open.CreateGUID, LeaseKey: open.LeaseKey, Binding: state.Binding{SessionID: 2, TreeID: 3},
	}
}

func TestDisconnectAndReconnectPreserveDurableState(t *testing.T) {
	table, now := clockTable(t)
	req := durableRequest(1, 2)
	req.SharingIntent = state.RightWrite | state.RightDelete
	req.GrantedAccess |= 0x10000
	grant := durableGrant(req)
	grant.DeleteOnClose, grant.DeleteName = true, deleteName("")
	open := commit(t, table, req, grant)
	ordinary := commit(t, table, request(2), state.Grant{})
	statusIs(t, table.SetDelete(open.ID, binding, grant.DeleteName, true), smb.StatusSuccess)
	statusIs(t, table.Lock(open.ID, binding, []state.Range{{Offset: 10, Length: 10, Exclusive: true}}, false), smb.StatusSuccess)
	actions := table.Disconnect(binding.SessionID)
	if len(actions) != 1 || actions[0].Handle != ordinary.Handle {
		t.Fatalf("disconnect cleanup: %+v", actions)
	}
	_, status := table.Find(open.ID, binding)
	statusIs(t, status, smb.StatusFileClosed)
	_, status = table.Reserve(request(1))
	statusIs(t, status, smb.StatusDeletePending)
	*now = now.Add(30 * time.Second)
	reattached, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	if reattached.ID.Persistent != open.ID.Persistent || reattached.ID.Volatile == open.ID.Volatile || reattached.Handle != open.Handle ||
		reattached.GrantedAccess != req.GrantedAccess || !reattached.DeleteOnClose || !reattached.Durable {
		t.Fatalf("reconnect changed the open: %+v", reattached)
	}
	_, status = table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusObjectNameNotFound)
	statusIs(t, table.Lock(reattached.ID, reattached.Binding, []state.Range{{Offset: 10, Length: 10}}, true), smb.StatusSuccess)
	if action := closeOpen(t, table, reattached); !action.Remove || action.Name != grant.DeleteName {
		t.Fatalf("close after reconnect: %+v", action)
	}
}

func TestReconnectRejectsEveryIdentityMismatch(t *testing.T) {
	for _, test := range []struct {
		modify func(*state.ReconnectRequest)
		name   string
		want   smb.Status
	}{
		{name: "persistent ID", modify: func(req *state.ReconnectRequest) { req.ID.Persistent++ }, want: smb.StatusObjectNameNotFound},
		{name: "volatile ID", modify: func(req *state.ReconnectRequest) { req.ID.Volatile++ }, want: smb.StatusObjectNameNotFound},
		{name: "share", modify: func(req *state.ReconnectRequest) { req.Share = "other" }, want: smb.StatusObjectNameNotFound},
		{name: "client", modify: func(req *state.ReconnectRequest) { req.ClientGUID[0]++ }, want: smb.StatusObjectNameNotFound},
		{name: "create GUID", modify: func(req *state.ReconnectRequest) { req.CreateGUID[0]++ }, want: smb.StatusObjectNameNotFound},
		{name: "lease key", modify: func(req *state.ReconnectRequest) { req.LeaseKey[0]++ }, want: smb.StatusObjectNameNotFound},
		{name: "user", modify: func(req *state.ReconnectRequest) { req.User = "other" }, want: smb.StatusAccessDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			table, _ := clockTable(t)
			req := durableRequest(1, 2)
			open := commit(t, table, req, durableGrant(req))
			good := reconnectRequest(open)
			_, status := table.Reconnect(good)
			statusIs(t, status, smb.StatusObjectNameNotFound)
			table.Disconnect(binding.SessionID)
			bad := good
			test.modify(&bad)
			_, status = table.Reconnect(bad)
			statusIs(t, status, test.want)
			_, status = table.Reconnect(good)
			statusIs(t, status, smb.StatusSuccess)
		})
	}
}

func TestExpiryClosesDetachedOpenAtItsDeadline(t *testing.T) {
	table, now := clockTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.DurableTimeout = 5 * time.Minute
	open := commit(t, table, req, grant)
	statusIs(t, table.Lock(open.ID, binding, []state.Range{{Length: 10, Exclusive: true}}, false), smb.StatusSuccess)
	table.Disconnect(binding.SessionID)
	*now = now.Add(time.Minute)
	table.Disconnect(binding.SessionID)
	*now = now.Add(4*time.Minute - time.Nanosecond)
	if actions := table.Expire(); len(actions) != 0 {
		t.Fatalf("expired early: %+v", actions)
	}
	*now = now.Add(time.Nanosecond)
	_, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusObjectNameNotFound)
	if actions := table.Expire(); len(actions) != 1 || actions[0].Handle != open.Handle {
		t.Fatalf("expiry cleanup: %+v", actions)
	}
	if actions := table.Expire(); len(actions) != 0 {
		t.Fatalf("expired twice: %+v", actions)
	}
	peer := commit(t, table, request(1), state.Grant{})
	statusIs(t, table.CheckIO(peer.ID, binding, 1, 1, true), smb.StatusSuccess)
}

func TestDurabilityNeedsHandleLeaseAndCreateGUID(t *testing.T) {
	for _, test := range []struct {
		modify func(*state.OpenRequest, *state.Grant)
		name   string
	}{
		{name: "no lease", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease = state.Lease{} }},
		{name: "no H", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.State = smb.LeaseRead }},
		{name: "no create GUID", modify: func(req *state.OpenRequest, _ *state.Grant) { req.CreateGUID = state.GUID{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			req := durableRequest(1, 2)
			grant := durableGrant(req)
			test.modify(&req, &grant)
			if open := commit(t, table, req, grant); open.Durable || open.DurableTimeout != 0 {
				t.Fatalf("durable without H or CREATE GUID: %+v", open)
			}
		})
	}
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.DurableTimeout = smb.MaxDurableTimeout + time.Nanosecond
	grant.Handle = &handle{key: req.Object}
	_, status := table.Commit(reserve(t, table, req), grant)
	statusIs(t, status, smb.StatusInvalidParameter)
}

func TestLogoffAndTreeDisconnectCloseDurableOpens(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	commit(t, table, req, durableGrant(req))
	otherTree := durableRequest(2, 3)
	otherTree.Binding.TreeID = 2
	commit(t, table, otherTree, durableGrant(otherTree))
	if actions := table.CloseTree(binding); len(actions) != 1 {
		t.Fatalf("tree disconnect cleanup: %+v", actions)
	}
	if actions := table.CloseSession(binding.SessionID); len(actions) != 1 {
		t.Fatalf("logoff cleanup: %+v", actions)
	}
}

func TestLockSequenceReplaysByNumber(t *testing.T) {
	table, _ := clockTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	ranges := []state.Range{{Length: 10, Exclusive: true}}
	// Index 1, number 0, then the same index with number 1.
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusSuccess)
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusSuccess)
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 17), smb.StatusLockNotGranted)
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusLockNotGranted)
	table.Disconnect(binding.SessionID)
	reattached, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	statusIs(t, table.LockSequence(reattached.ID, reattached.Binding, ranges, true, 32), smb.StatusSuccess)
	statusIs(t, table.LockSequence(reattached.ID, reattached.Binding, ranges, true, 32), smb.StatusSuccess)
	// Index 0 is not a replay slot, and ordinary opens never replay.
	statusIs(t, table.LockSequence(reattached.ID, reattached.Binding, ranges, false, 0), smb.StatusSuccess)
	statusIs(t, table.LockSequence(reattached.ID, reattached.Binding, ranges, false, 0), smb.StatusLockNotGranted)
	ordinary := commit(t, table, request(2), state.Grant{})
	statusIs(t, table.LockSequence(ordinary.ID, binding, ranges, false, 16), smb.StatusSuccess)
	statusIs(t, table.LockSequence(ordinary.ID, binding, ranges, false, 16), smb.StatusLockNotGranted)
}
