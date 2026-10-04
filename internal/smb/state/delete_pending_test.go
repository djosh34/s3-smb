package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestDeletePendingSnapshot(t *testing.T) {
	table := newTable(t)
	base := commit(t, table, deleteRequest(1, ""), state.Grant{})
	stream := commit(t, table, deleteRequest(1, "fork"), state.Grant{})
	other := smb.ObjectKey{Inode: 1, Stream: "other"}
	if table.DeletePending(base.Object) || table.DeletePending(stream.Object) || table.DeletePending(smb.ObjectKey{Inode: 2}) {
		t.Fatal("new objects are delete pending")
	}
	statusIs(t, table.SetDelete(stream.ID, binding, deleteName("fork"), true), smb.StatusSuccess)
	if !table.DeletePending(stream.Object) || table.DeletePending(base.Object) || table.DeletePending(other) {
		t.Fatal("stream deletion escaped its object")
	}
	statusIs(t, table.SetDelete(stream.ID, binding, smb.Name{}, false), smb.StatusSuccess)
	statusIs(t, table.SetDelete(base.ID, binding, deleteName(""), true), smb.StatusSuccess)
	if !table.DeletePending(base.Object) || !table.DeletePending(stream.Object) || !table.DeletePending(other) {
		t.Fatal("base deletion did not cover its streams")
	}
	statusIs(t, table.SetDelete(base.ID, binding, smb.Name{}, false), smb.StatusSuccess)
	if table.DeletePending(base.Object) || table.DeletePending(stream.Object) {
		t.Fatal("cleared deletion remains pending")
	}
}
