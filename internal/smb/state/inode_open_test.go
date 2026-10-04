package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestInodeOpenIncludesReservationsAndStreams(t *testing.T) {
	for _, stream := range []string{"", "user.resource"} {
		t.Run(stream, func(t *testing.T) {
			table := newTable(t)
			req := request(17)
			req.Object.Stream = stream
			if table.InodeOpen(17) {
				t.Fatal("unused inode is open")
			}
			token := reserve(t, table, req)
			if !table.InodeOpen(17) || table.InodeOpen(18) {
				t.Fatal("reservation is not isolated to its inode")
			}
			statusIs(t, table.Abort(token), smb.StatusSuccess)
			if table.InodeOpen(17) {
				t.Fatal("aborted reservation still counts as an open")
			}
			open := commit(t, table, req, state.Grant{})
			if !table.InodeOpen(17) || table.InodeOpen(18) {
				t.Fatal("committed open is not isolated to its inode")
			}
			_, status := table.Close(open.ID, binding)
			statusIs(t, status, smb.StatusSuccess)
			if table.InodeOpen(17) {
				t.Fatal("closed inode is still open")
			}
		})
	}
}

func TestInodeOpenIncludesDetachedDurable(t *testing.T) {
	table := newTable(t)
	req := durableRequest(17, 2)
	commit(t, table, req, durableGrant(req))
	if actions := table.Disconnect(binding.SessionID); len(actions) != 0 {
		t.Fatalf("disconnect cleanup = %+v", actions)
	}
	if !table.InodeOpen(17) {
		t.Fatal("detached durable open did not count")
	}
	table.CloseAll()
	if table.InodeOpen(17) {
		t.Fatal("closed durable inode is still open")
	}
}
