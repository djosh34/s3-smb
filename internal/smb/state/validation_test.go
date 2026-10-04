package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestReplayChecksAllOriginalParameters(t *testing.T) {
	for _, test := range []struct {
		modify func(*state.OpenRequest)
		name   string
	}{
		{name: "object", modify: func(req *state.OpenRequest) { req.Object.Inode++ }},
		{name: "stream", modify: func(req *state.OpenRequest) { req.Object.Stream = "xattr" }},
		{name: "sharing", modify: func(req *state.OpenRequest) { req.Sharing = 0 }},
		{name: "intent", modify: func(req *state.OpenRequest) { req.SharingIntent = state.RightWrite }},
		{name: "parameters", modify: func(req *state.OpenRequest) { req.CreateParameters[0]++ }},
		{name: "session", modify: func(req *state.OpenRequest) { req.Binding.SessionID++ }},
		{name: "tree", modify: func(req *state.OpenRequest) { req.Binding.TreeID++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			req := request(1)
			req.CreateGUID = state.GUID{2}
			open := commit(t, table, req, state.Grant{})
			bad := req
			test.modify(&bad)
			_, status := table.Replay(bad)
			statusIs(t, status, smb.StatusInvalidParameter)
			found, status := table.Replay(req)
			statusIs(t, status, smb.StatusSuccess)
			if found != open {
				t.Fatal("failed replay changed original")
			}
		})
	}
}

func TestCreateIdentityIncludesClientUserAndShare(t *testing.T) {
	table := newTable(t)
	req := request(1)
	req.CreateGUID = state.GUID{2}
	first := commit(t, table, req, state.Grant{})
	for _, modify := range []func(*state.OpenRequest){
		func(req *state.OpenRequest) { req.ClientGUID = state.GUID{9} },
		func(req *state.OpenRequest) { req.User = "other" },
		func(req *state.OpenRequest) { req.Share = "other" },
	} {
		otherReq := req
		modify(&otherReq)
		other := commit(t, table, otherReq, state.Grant{})
		if other.ID == first.ID {
			t.Fatal("unrelated CREATE reused an open")
		}
	}
	closeOpen(t, table, first)
	commit(t, table, req, state.Grant{})
}

func TestReservationConsumedOnlyOnce(t *testing.T) {
	table := newTable(t)
	req := request(1)
	token := reserve(t, table, req)
	grant := state.Grant{Handle: &handle{key: req.Object}}
	open, status := table.Commit(token, grant)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.Commit(token, grant)
	statusIs(t, status, smb.StatusInvalidParameter)
	statusIs(t, table.Abort(token), smb.StatusInvalidParameter)
	closeOpen(t, table, open)
	_, status = table.Commit(0, grant)
	statusIs(t, status, smb.StatusInvalidParameter)
}

func TestStaleBindingCannotChangeAnOpen(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	req.GrantedAccess |= 0x10000
	open := commit(t, table, req, durableGrant(req))
	table.Disconnect(1)
	fresh, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	statusIs(t, table.SetDelete(open.ID, binding, deleteName(""), true), smb.StatusFileClosed)
	statusIs(t, table.SetDirectory(open.ID, binding, state.DirectoryCursor{Pattern: "*"}), smb.StatusFileClosed)
	statusIs(t, table.Lock(open.ID, binding, []state.Range{{Length: 10}}, false), smb.StatusFileClosed)
	statusIs(t, table.CheckIO(open.ID, binding, 0, 10, false), smb.StatusFileClosed)
	_, status = table.Close(open.ID, binding)
	statusIs(t, status, smb.StatusFileClosed)
	found, status := table.Find(fresh.ID, fresh.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if found != fresh {
		t.Fatal("stale requests changed the reconnected open")
	}
}

func TestCommitDeleteOnCloseChecksSharing(t *testing.T) {
	table := newTable(t)
	deny := requestWithStream(1, "xattr")
	deny.Sharing = state.ShareMode(state.RightRead | state.RightWrite)
	commit(t, table, deny, state.Grant{})
	req := request(1)
	req.GrantedAccess = 0x10000
	token := reserve(t, table, req)
	_, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, DeleteOnClose: true, DeleteName: deleteName("")})
	statusIs(t, status, smb.StatusSharingViolation)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	reserve(t, table, request(1))
}

func TestReturnedOpenCannotMutateTable(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, request(1), state.Grant{})
	found, status := table.Find(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	found.GrantedAccess = 0xffffffff
	found.Sharing = 0
	found.Directory.Pattern = "changed"
	found.DeleteOnClose = true
	unchanged, status := table.Find(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if unchanged != open || unchanged == found {
		t.Fatal("returned open aliases table storage")
	}
}
