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
	if len(actions) != 1 || actions[0].Handle != ordinary.Handle || actions[0].Remove {
		t.Fatalf("disconnect cleanup: %+v", actions)
	}
	_, status := table.Find(open.ID, binding)
	statusIs(t, status, smb.StatusFileClosed)
	_, status = table.Find(open.ID, state.Binding{})
	statusIs(t, status, smb.StatusFileClosed)
	_, status = table.Reserve(request(1))
	statusIs(t, status, smb.StatusDeletePending)
	*now = now.Add(30 * time.Second)
	reattached, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	if reattached.ID.Persistent != open.ID.Persistent || reattached.ID.Volatile == open.ID.Volatile || reattached.Handle != open.Handle ||
		reattached.GrantedAccess != req.GrantedAccess || reattached.SharingIntent != req.SharingIntent || !reattached.DeleteOnClose || !reattached.Durable || !reattached.DurableDeadline.IsZero() {
		t.Fatalf("reconnect changed retained state: %+v", reattached)
	}
	_, status = table.Find(open.ID, reattached.Binding)
	statusIs(t, status, smb.StatusFileClosed)
	statusIs(t, table.Lock(reattached.ID, reattached.Binding, []state.Range{{Offset: 10, Length: 10}}, true), smb.StatusSuccess)
	replay := req
	replay.Binding = reattached.Binding
	found, status := table.Replay(replay)
	statusIs(t, status, smb.StatusSuccess)
	if found != reattached {
		t.Fatal("replay lost reconnected state")
	}
	action := closeOpen(t, table, reattached)
	if !action.Remove || action.Name != grant.DeleteName {
		t.Fatalf("reconnected deletion cleanup: %+v", action)
	}
}

func TestDetachedOpensRetainSharingAndRanges(t *testing.T) {
	table, _ := clockTable(t)
	req := durableRequest(1, 2)
	req.Sharing = state.ShareMode(state.RightRead | state.RightDelete)
	open := commit(t, table, req, durableGrant(req))
	other := commit(t, table, request(1), state.Grant{})
	statusIs(t, table.Lock(open.ID, binding, []state.Range{{Length: 10, Exclusive: true}}, false), smb.StatusSuccess)
	if len(table.Disconnect(1)) != 1 {
		t.Fatal("ordinary open did not close")
	}
	otherReq := request(1)
	otherReq.Binding = state.Binding{SessionID: 2, TreeID: 1}
	otherReq.SharingIntent = state.RightWrite
	_, status := table.Reserve(otherReq)
	statusIs(t, status, smb.StatusSharingViolation)
	otherReq.SharingIntent = state.RightRead
	other = commit(t, table, otherReq, state.Grant{})
	statusIs(t, table.CheckIO(other.ID, other.Binding, 1, 1, false), smb.StatusFileLockConflict)
	statusIs(t, table.Lock(other.ID, other.Binding, []state.Range{{Length: 10, Exclusive: true}}, false), smb.StatusLockNotGranted)
}

func TestReconnectRejectsEveryIdentityMismatch(t *testing.T) {
	for _, test := range []struct {
		modify func(*state.ReconnectRequest)
		name   string
	}{
		{name: "persistent ID", modify: func(req *state.ReconnectRequest) { req.ID.Persistent++ }},
		{name: "volatile ID", modify: func(req *state.ReconnectRequest) { req.ID.Volatile++ }},
		{name: "user", modify: func(req *state.ReconnectRequest) { req.User = "other" }},
		{name: "share", modify: func(req *state.ReconnectRequest) { req.Share = "other" }},
		{name: "client", modify: func(req *state.ReconnectRequest) { req.ClientGUID[0]++ }},
		{name: "create", modify: func(req *state.ReconnectRequest) { req.CreateGUID[0]++ }},
		{name: "lease", modify: func(req *state.ReconnectRequest) { req.LeaseKey[0]++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			table, _ := clockTable(t)
			req := durableRequest(1, 2)
			open := commit(t, table, req, durableGrant(req))
			good := reconnectRequest(open)
			_, status := table.Reconnect(good)
			statusIs(t, status, smb.StatusObjectNameNotFound)
			table.Disconnect(1)
			bad := good
			test.modify(&bad)
			_, status = table.Reconnect(bad)
			want := smb.StatusObjectNameNotFound
			if test.name == "user" {
				want = smb.StatusAccessDenied
			}
			statusIs(t, status, want)
			_, status = table.Reconnect(good)
			statusIs(t, status, smb.StatusSuccess)
			_, status = table.Reconnect(good)
			statusIs(t, status, smb.StatusObjectNameNotFound)
		})
	}
}

func TestFakeClockExpiryUsesClosePath(t *testing.T) {
	table, now := clockTable(t)
	req := durableRequest(1, 2)
	req.GrantedAccess |= 0x10000
	req.SharingIntent |= state.RightDelete
	grant := durableGrant(req)
	grant.DeleteOnClose, grant.DeleteName = true, deleteName("")
	open := commit(t, table, req, grant)
	statusIs(t, table.Lock(open.ID, binding, []state.Range{{Length: 10, Exclusive: true}}, false), smb.StatusSuccess)
	if actions := table.Disconnect(1); len(actions) != 0 {
		t.Fatalf("durable closed on drop: %+v", actions)
	}
	*now = now.Add(smb.DefaultDurableTimeout - time.Nanosecond)
	if len(table.Expire()) != 0 {
		t.Fatal("expired before deadline")
	}
	*now = now.Add(time.Nanosecond)
	_, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusObjectNameNotFound)
	actions := table.Expire()
	if len(actions) != 1 || actions[0].FileID != open.ID || actions[0].Handle != open.Handle || !actions[0].Remove || actions[0].Name != grant.DeleteName {
		t.Fatalf("expiry cleanup: %+v", actions)
	}
	if len(table.Expire()) != 0 || len(table.CloseAll()) != 0 {
		t.Fatal("expired twice")
	}
	fresh := commit(t, table, request(1), state.Grant{})
	statusIs(t, table.CheckIO(fresh.ID, binding, 1, 1, true), smb.StatusSuccess)
	commit(t, table, req, durableGrant(req))
}

func TestDisconnectStartsTimeoutOnlyOnce(t *testing.T) {
	table, now := clockTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	table.Disconnect(1)
	*now = now.Add(time.Minute)
	table.Disconnect(1)
	if len(table.Disconnect(0)) != 0 || len(table.CloseSession(0)) != 0 || len(table.CloseTree(state.Binding{})) != 0 {
		t.Fatal("zero binding acted on detached opens")
	}
	*now = now.Add(time.Minute)
	actions := table.Expire()
	if len(actions) != 1 || actions[0].Handle != open.Handle {
		t.Fatalf("disconnect extended deadline: %+v", actions)
	}
}

func TestCloseTreeClosesOrdinaryAndDurableOpens(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	commit(t, table, req, durableGrant(req))
	commit(t, table, request(2), state.Grant{})
	otherTree := request(3)
	otherTree.Binding.TreeID = 2
	survivor := commit(t, table, otherTree, state.Grant{})
	reservation := reserve(t, table, request(4))
	actions := table.CloseTree(binding)
	if len(actions) != 2 {
		t.Fatalf("tree close returned %d actions", len(actions))
	}
	if len(table.CloseTree(binding)) != 0 {
		t.Fatal("tree close repeated cleanup")
	}
	_, status := table.Find(survivor.ID, survivor.Binding)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.Commit(reservation, state.Grant{Handle: &handle{key: smb.ObjectKey{Inode: 4}}})
	statusIs(t, status, smb.StatusInvalidParameter)
}

func TestCloseSessionAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		table := newTable(t)
		for inode := smb.Inode(1); inode <= 2; inode++ {
			req := durableRequest(inode, byte(inode))
			req.Binding.TreeID = uint32(inode)
			commit(t, table, req, durableGrant(req))
		}
		otherReq := durableRequest(3, 3)
		otherReq.Binding.SessionID = 2
		other := commit(t, table, otherReq, durableGrant(otherReq))
		var actions []state.CloseAction
		if shutdown {
			table.Disconnect(2)
			actions = table.CloseAll()
			if len(actions) != 3 || len(table.CloseAll()) != 0 {
				t.Fatalf("shutdown cleanup: %+v", actions)
			}
		} else {
			actions = table.CloseSession(1)
			if len(actions) != 2 || len(table.CloseSession(1)) != 0 {
				t.Fatalf("logoff cleanup: %+v", actions)
			}
			_, status := table.Find(other.ID, other.Binding)
			statusIs(t, status, smb.StatusSuccess)
		}
	}
}

func TestDisconnectAbortsUncommittedCreates(t *testing.T) {
	table := newTable(t)
	req := request(1)
	req.CreateGUID = state.GUID{2}
	req.Sharing = 0
	token := reserve(t, table, req)
	if len(table.Disconnect(1)) != 0 {
		t.Fatal("reservation produced storage cleanup")
	}
	_, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}})
	statusIs(t, status, smb.StatusInvalidParameter)
	reserve(t, table, req)
}

func TestDurableGrantsRequireRegularFileAndH(t *testing.T) {
	for _, test := range []struct {
		modify func(*state.OpenRequest, *state.Grant)
		name   string
	}{
		{name: "without H", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.State = smb.LeaseRead }},
		{name: "directory", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Directory = true }},
		{name: "named stream", modify: func(req *state.OpenRequest, _ *state.Grant) { req.Object.Stream = "xattr" }},
		{name: "missing create GUID", modify: func(req *state.OpenRequest, _ *state.Grant) { req.CreateGUID = state.GUID{} }},
		{name: "over maximum", modify: func(_ *state.OpenRequest, grant *state.Grant) {
			grant.DurableTimeout = smb.MaxDurableTimeout + time.Nanosecond
		}},
		{name: "negative", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.DurableTimeout = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			req := durableRequest(1, 2)
			grant := durableGrant(req)
			test.modify(&req, &grant)
			grant.Handle = &handle{key: req.Object}
			token := reserve(t, table, req)
			_, status := table.Commit(token, grant)
			statusIs(t, status, smb.StatusInvalidParameter)
			statusIs(t, table.Abort(token), smb.StatusSuccess)
		})
	}
}

func TestGrantedTimeoutExpiresExactly(t *testing.T) {
	for _, timeout := range []time.Duration{time.Millisecond, smb.DefaultDurableTimeout, 5 * time.Minute, smb.MaxDurableTimeout} {
		t.Run(timeout.String(), func(t *testing.T) {
			table, now := clockTable(t)
			req := durableRequest(1, 2)
			grant := durableGrant(req)
			grant.DurableTimeout = timeout
			open := commit(t, table, req, grant)
			if open.DurableTimeout != timeout {
				t.Fatalf("granted timeout = %v", open.DurableTimeout)
			}
			table.Disconnect(1)
			*now = now.Add(timeout - time.Nanosecond)
			if len(table.Expire()) != 0 {
				t.Fatal("expired early")
			}
			*now = now.Add(time.Nanosecond)
			if len(table.Expire()) != 1 {
				t.Fatal("did not expire at deadline")
			}
		})
	}
}
