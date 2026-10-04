package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestBulkCloseKeepsDeletePendingUntilCleanup(t *testing.T) {
	for _, method := range []string{"logoff", "tree disconnect", "drop", "shutdown"} {
		t.Run(method, func(t *testing.T) {
			table := newTable(t)
			open := commit(t, table, deleteRequest(1, ""), state.Grant{DeleteOnClose: true, DeleteName: deleteName("")})
			var actions []state.CloseAction
			switch method {
			case "logoff":
				actions = table.CloseSession(open.Binding.SessionID)
			case "tree disconnect":
				actions = table.CloseTree(open.Binding)
			case "drop":
				actions = table.Disconnect(open.Binding.SessionID)
			case "shutdown":
				actions = table.CloseAll()
			}
			if len(actions) != 1 || !actions[0].Remove {
				t.Fatalf("cleanup = %+v", actions)
			}
			_, status := table.Reserve(request(1))
			statusIs(t, status, smb.StatusDeletePending)
			_, status = table.Reserve(requestWithStream(1, "xattr"))
			statusIs(t, status, smb.StatusDeletePending)
			table.CompleteDelete(actions[0].Object)
			commit(t, table, request(1), state.Grant{})
		})
	}
}

func TestDeleteCleanupBlocksStreamButNotBase(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, deleteRequest(1, "xattr"), state.Grant{DeleteOnClose: true, DeleteName: deleteName("xattr")})
	action := closeOpen(t, table, open)
	if !action.Remove {
		t.Fatal("stream close did not request removal")
	}
	_, status := table.Reserve(requestWithStream(1, "xattr"))
	statusIs(t, status, smb.StatusDeletePending)
	commit(t, table, request(1), state.Grant{})
	commit(t, table, requestWithStream(1, "other"), state.Grant{})
	table.CompleteDelete(action.Object)
	commit(t, table, requestWithStream(1, "xattr"), state.Grant{})
}

func TestDeletionRejectsReservationCommitDuringCleanup(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, deleteRequest(1, ""), state.Grant{DeleteOnClose: true, DeleteName: deleteName("")})
	token := reserve(t, table, request(1))
	action := closeOpen(t, table, open)
	_, status := table.Commit(token, state.Grant{Handle: &handle{key: open.Object}})
	statusIs(t, status, smb.StatusDeletePending)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	table.CompleteDelete(action.Object)
	commit(t, table, request(1), state.Grant{})
}
