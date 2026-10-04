package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func deleteRequest(inode smb.Inode, stream string) state.OpenRequest {
	req := request(inode)
	req.Object.Stream = stream
	req.GrantedAccess = 0x10000
	req.SharingIntent = state.RightDelete
	return req
}

func deleteName(stream string) smb.Name {
	return smb.Name{Parent: 1, Base: "backup", Stream: stream}
}

func closeOpen(t *testing.T, table *state.Table, open state.Open) state.CloseAction {
	t.Helper()
	action, status := table.Close(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if action.Handle != open.Handle {
		t.Fatal("close lost storage handle")
	}
	_, status = table.Find(open.ID, open.Binding)
	statusIs(t, status, smb.StatusFileClosed)
	return action
}

func TestDeletePendingAndClearingIndependentIntent(t *testing.T) {
	table := newTable(t)
	first := commit(t, table, deleteRequest(1, ""), state.Grant{})
	second := commit(t, table, deleteRequest(1, ""), state.Grant{})
	name := deleteName("")
	statusIs(t, table.SetDelete(first.ID, binding, name, true), smb.StatusSuccess)
	_, status := table.Reserve(request(1))
	statusIs(t, status, smb.StatusDeletePending)
	statusIs(t, table.SetDelete(second.ID, binding, name, true), smb.StatusSuccess)
	statusIs(t, table.SetDelete(first.ID, binding, name, false), smb.StatusSuccess)
	_, status = table.Reserve(request(1))
	statusIs(t, status, smb.StatusDeletePending)
	if action := closeOpen(t, table, first); action.Remove {
		t.Fatal("deletion ran while another open remained")
	}
	action := closeOpen(t, table, second)
	if !action.Remove || action.Object != second.Object || action.Name != name {
		t.Fatalf("last close cleanup = %+v", action)
	}
	commit(t, table, request(1), state.Grant{})
}

func TestCreateDeleteOnCloseBecomesPendingOnlyAtClose(t *testing.T) {
	table := newTable(t)
	first := commit(t, table, deleteRequest(1, ""), state.Grant{DeleteOnClose: true, DeleteName: deleteName("")})
	second := commit(t, table, request(1), state.Grant{})
	denyDelete := request(1)
	denyDelete.GrantedAccess = 1
	denyDelete.Sharing = state.ShareMode(state.RightRead | state.RightWrite)
	_, status := table.Reserve(denyDelete)
	statusIs(t, status, smb.StatusSharingViolation)
	if action := closeOpen(t, table, first); action.Remove {
		t.Fatal("CREATE delete-on-close removed a live file")
	}
	_, status = table.Reserve(request(1))
	statusIs(t, status, smb.StatusDeletePending)
	action := closeOpen(t, table, second)
	if !action.Remove || action.Name != deleteName("") {
		t.Fatalf("CREATE delete-on-close cleanup: %+v", action)
	}
}

func TestClearingDispositionDoesNotClearCreateDeleteOnClose(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, deleteRequest(1, ""), state.Grant{DeleteOnClose: true, DeleteName: deleteName("")})
	statusIs(t, table.SetDelete(open.ID, binding, deleteName(""), true), smb.StatusSuccess)
	_, status := table.Reserve(request(1))
	statusIs(t, status, smb.StatusDeletePending)
	statusIs(t, table.SetDelete(open.ID, binding, smb.Name{}, false), smb.StatusSuccess)
	other := commit(t, table, request(1), state.Grant{})
	closeOpen(t, table, other)
	if action := closeOpen(t, table, open); !action.Remove || action.Name != deleteName("") {
		t.Fatalf("clearing disposition lost CREATE deletion: %+v", action)
	}
}

func TestClearingLastDeleteIntentAllowsOpens(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, deleteRequest(1, ""), state.Grant{})
	statusIs(t, table.SetDelete(open.ID, binding, deleteName(""), true), smb.StatusSuccess)
	statusIs(t, table.SetDelete(open.ID, binding, smb.Name{}, false), smb.StatusSuccess)
	commit(t, table, request(1), state.Grant{})
	if action := closeOpen(t, table, open); action.Remove {
		t.Fatal("cleared intent caused deletion")
	}
}

func TestClosedDeleteIntentCannotBeClearedByAnotherOpen(t *testing.T) {
	table := newTable(t)
	first := commit(t, table, deleteRequest(1, ""), state.Grant{})
	second := commit(t, table, deleteRequest(1, ""), state.Grant{})
	selected := deleteName("")
	statusIs(t, table.SetDelete(first.ID, binding, selected, true), smb.StatusSuccess)
	if action := closeOpen(t, table, first); action.Remove {
		t.Fatal("first close removed a live inode")
	}
	statusIs(t, table.SetDelete(second.ID, binding, smb.Name{}, false), smb.StatusSuccess)
	_, status := table.Reserve(request(1))
	statusIs(t, status, smb.StatusDeletePending)
	action := closeOpen(t, table, second)
	if !action.Remove || action.Name != selected {
		t.Fatalf("pending name was lost: %+v", action)
	}
}

func TestStreamDeletionIsIsolated(t *testing.T) {
	table := newTable(t)
	base := commit(t, table, request(1), state.Grant{})
	first := commit(t, table, deleteRequest(1, "xattr.one"), state.Grant{})
	second := commit(t, table, deleteRequest(1, "xattr.two"), state.Grant{})
	statusIs(t, table.SetDelete(first.ID, binding, deleteName("xattr.one"), true), smb.StatusSuccess)
	_, status := table.Reserve(deleteRequest(1, "xattr.one"))
	statusIs(t, status, smb.StatusDeletePending)
	reserve(t, table, deleteRequest(1, "xattr.two"))
	reserve(t, table, request(1))
	action := closeOpen(t, table, first)
	if !action.Remove || action.Object != first.Object || action.Name.Stream != "xattr.one" {
		t.Fatalf("stream deletion: %+v", action)
	}
	if action = closeOpen(t, table, second); action.Remove {
		t.Fatal("deleted unrelated stream")
	}
	if action = closeOpen(t, table, base); action.Remove {
		t.Fatal("deleted base with stream")
	}
}

func TestBaseDeletionWaitsForLastStream(t *testing.T) {
	table := newTable(t)
	base := commit(t, table, deleteRequest(1, ""), state.Grant{})
	stream := commit(t, table, requestWithStream(1, "xattr"), state.Grant{})
	statusIs(t, table.SetDelete(base.ID, binding, deleteName(""), true), smb.StatusSuccess)
	_, status := table.Reserve(requestWithStream(1, "another"))
	statusIs(t, status, smb.StatusDeletePending)
	if action := closeOpen(t, table, base); action.Remove {
		t.Fatal("base removed before stream closed")
	}
	action := closeOpen(t, table, stream)
	if !action.Remove || action.Object != base.Object || action.Name != deleteName("") || action.Handle != stream.Handle {
		t.Fatalf("base deletion on stream close: %+v", action)
	}
}

func requestWithStream(inode smb.Inode, stream string) state.OpenRequest {
	req := request(inode)
	req.Object.Stream = stream
	return req
}

func TestBaseDeleteSharingChecksEveryStream(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, committed := range []bool{false, true} {
			table := newTable(t)
			deny := requestWithStream(1, "xattr")
			deny.Sharing = state.ShareMode(state.RightRead | state.RightWrite)
			need := deleteRequest(1, "")
			first, second := deny, need
			if reverse {
				first, second = need, deny
			}
			if committed {
				commit(t, table, first, state.Grant{})
			} else {
				reserve(t, table, first)
			}
			_, status := table.Reserve(second)
			statusIs(t, status, smb.StatusSharingViolation)
			reserve(t, table, deleteRequest(2, ""))
		}
	}
}

func TestDeleteRequiresAccessAndCompatibleSharing(t *testing.T) {
	table := newTable(t)
	metadata := commit(t, table, request(1), state.Grant{})
	statusIs(t, table.SetDelete(metadata.ID, binding, deleteName(""), true), smb.StatusAccessDenied)
	req := request(2)
	req.GrantedAccess = 0x10000
	open := commit(t, table, req, state.Grant{})
	deny := requestWithStream(2, "xattr")
	deny.Sharing = state.ShareMode(state.RightRead | state.RightWrite)
	_, status := table.Reserve(deny)
	statusIs(t, status, smb.StatusSharingViolation)
	statusIs(t, table.SetDelete(open.ID, binding, deleteName(""), true), smb.StatusSuccess)
	token := reserve(t, table, request(3))
	_, status = table.Commit(token, state.Grant{Handle: &handle{key: smb.ObjectKey{Inode: 3}}, DeleteOnClose: true, DeleteName: deleteName("")})
	statusIs(t, status, smb.StatusAccessDenied)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
}

func TestDeleteNameMustSelectTheSameStream(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, deleteRequest(1, "xattr"), state.Grant{})
	statusIs(t, table.SetDelete(open.ID, binding, deleteName(""), true), smb.StatusInvalidParameter)
	statusIs(t, table.SetDelete(open.ID, binding, smb.Name{Base: "backup", Stream: "xattr"}, true), smb.StatusInvalidParameter)
	reserve(t, table, requestWithStream(1, "xattr"))
}
