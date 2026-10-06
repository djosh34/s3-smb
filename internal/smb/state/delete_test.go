package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func deleteRequest() state.OpenRequest {
	req := request(1)
	req.GrantedAccess = 0x10000
	req.SharingIntent = state.RightDelete
	return req
}

func deleteName() smb.Name {
	return smb.Name{Parent: 1, Base: "backup"}
}

func closeOpen(t *testing.T, table *state.Table, open state.Open) state.CloseAction {
	t.Helper()
	action, status := table.Close(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if action.FileID != open.ID || action.Handle != open.Handle {
		t.Fatal("close lost open identity or storage handle")
	}
	_, status = table.Find(open.ID, open.Binding)
	statusIs(t, status, smb.StatusFileClosed)
	return action
}

func TestCreateDeleteOnCloseBecomesPendingOnlyAtClose(t *testing.T) {
	table := newTable(t)
	first := commit(t, table, deleteRequest(), state.Grant{DeleteOnClose: true, DeleteName: deleteName()})
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
	if !action.Remove || action.Name != deleteName() {
		t.Fatalf("CREATE delete-on-close cleanup: %+v", action)
	}
}

func TestClosedDeleteIntentCannotBeClearedByAnotherOpen(t *testing.T) {
	table := newTable(t)
	first := commit(t, table, deleteRequest(), state.Grant{})
	second := commit(t, table, deleteRequest(), state.Grant{})
	selected := deleteName()
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

func TestDeleteNameMustBeComplete(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, deleteRequest(), state.Grant{})
	statusIs(t, table.SetDelete(open.ID, binding, smb.Name{Base: "backup"}, true), smb.StatusInvalidParameter)
	statusIs(t, table.SetDelete(open.ID, binding, smb.Name{Parent: 1}, true), smb.StatusInvalidParameter)
	statusIs(t, table.SetDelete(open.ID, binding, deleteName(), true), smb.StatusSuccess)
}

// However the opens of a session or tree end, a file deleted on close stays
// delete pending until storage has removed it.
func TestBulkCloseKeepsDeletePendingUntilCleanup(t *testing.T) {
	for _, end := range []func(*state.Table) []state.CloseAction{
		func(table *state.Table) []state.CloseAction { return table.CloseSession(binding.SessionID) },
		func(table *state.Table) []state.CloseAction { return table.CloseTree(binding) },
		func(table *state.Table) []state.CloseAction { return table.Disconnect(binding.SessionID) },
		(*state.Table).CloseAll,
	} {
		table := newTable(t)
		commit(t, table, deleteRequest(), state.Grant{DeleteOnClose: true, DeleteName: deleteName()})
		actions := end(table)
		if len(actions) != 1 || !actions[0].Remove {
			t.Fatalf("cleanup = %+v", actions)
		}
		_, status := table.Reserve(request(1))
		statusIs(t, status, smb.StatusDeletePending)
		table.CompleteDelete(actions[0].Object)
		commit(t, table, request(1), state.Grant{})
	}
}

// A reservation made before the last close cannot commit while the delete
// runs.
func TestDeletionRejectsCommitDuringCleanup(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, deleteRequest(), state.Grant{DeleteOnClose: true, DeleteName: deleteName()})
	token := reserve(t, table, request(1))
	action := closeOpen(t, table, open)
	_, status := table.Commit(token, state.Grant{Handle: &handle{key: open.Object}})
	statusIs(t, status, smb.StatusDeletePending)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	table.CompleteDelete(action.Object)
	commit(t, table, request(1), state.Grant{})
}
