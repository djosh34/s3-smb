package state_test

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

const shareAll = state.ShareMode(state.RightRead | state.RightWrite | state.RightDelete)

var binding = state.Binding{SessionID: 1, TreeID: 1}

type handle struct{ key smb.ObjectKey }

func (reference *handle) Key() smb.ObjectKey { return reference.key }

func newTable(t *testing.T) *state.Table {
	t.Helper()
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func request(inode smb.Inode) state.OpenRequest {
	return state.OpenRequest{
		User: "user", Share: "share", Object: smb.ObjectKey{Inode: inode}, Binding: binding,
		ClientGUID: state.GUID{1}, Sharing: shareAll,
	}
}

func statusIs(t *testing.T, got, want smb.Status) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %#x, want %#x", got, want)
	}
}

func reserve(t *testing.T, table *state.Table, request state.OpenRequest) state.Reservation {
	t.Helper()
	token, status := table.Reserve(request)
	statusIs(t, status, smb.StatusSuccess)
	return token
}

func commit(t *testing.T, table *state.Table, request state.OpenRequest, grant state.Grant) state.Open {
	t.Helper()
	if grant.Handle == nil {
		grant.Handle = &handle{key: request.Object}
	}
	open, status := table.Commit(reserve(t, table, request), grant)
	statusIs(t, status, smb.StatusSuccess)
	return open
}

func TestNewRequiresClock(t *testing.T) {
	if table, err := state.New(nil); err == nil || table != nil {
		t.Fatalf("New(nil) = %v, %v", table, err)
	}
}

func TestShareChecksBothDirections(t *testing.T) {
	for _, rights := range []state.Rights{state.RightRead, state.RightWrite, state.RightDelete} {
		t.Run(rightsName(rights), func(t *testing.T) {
			for _, committed := range []bool{false, true} {
				for _, reverse := range []bool{false, true} {
					table := newTable(t)
					deny, need := request(1), request(1)
					deny.Sharing = state.ShareMode(state.Rights(shareAll) & ^rights)
					need.SharingIntent = rights
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
				}
			}
		})
	}
}

func rightsName(right state.Rights) string {
	switch right {
	case state.RightRead:
		return "read"
	case state.RightWrite:
		return "write"
	case state.RightDelete:
		return "delete"
	}
	return "invalid"
}

func TestReservationRollback(t *testing.T) {
	table := newTable(t)
	first := request(1)
	first.CreateGUID = state.GUID{2}
	first.SharingIntent, first.Sharing = state.RightWrite, 0
	token := reserve(t, table, first)
	_, status := table.Commit(token, state.Grant{})
	statusIs(t, status, smb.StatusInvalidParameter)
	conflicting := request(1)
	conflicting.SharingIntent = state.RightRead
	_, status = table.Reserve(conflicting)
	statusIs(t, status, smb.StatusSharingViolation)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	statusIs(t, table.Abort(token), smb.StatusInvalidParameter)
	reserve(t, table, first)
}

func TestFullGrantedAccessSurvivesReplay(t *testing.T) {
	for _, access := range []struct {
		name   string
		mask   uint32
		intent state.Rights
	}{
		{name: "append only", mask: 0x00120104, intent: state.RightWrite},
		{name: "write data", mask: 0x00120102, intent: state.RightWrite},
		{name: "metadata only", mask: 0x00120080},
	} {
		t.Run(access.name, func(t *testing.T) {
			table := newTable(t)
			req := request(1)
			req.CreateGUID = state.GUID{2}
			req.GrantedAccess, req.SharingIntent = access.mask, access.intent
			original := commit(t, table, req, state.Grant{})
			if original.GrantedAccess != access.mask || original.SharingIntent != access.intent {
				t.Fatalf("grant changed access: %+v", original)
			}
			replayed, status := table.Replay(req)
			statusIs(t, status, smb.StatusSuccess)
			if replayed != original {
				t.Fatalf("replay changed open: %+v", replayed)
			}
			_, status = table.Reserve(req)
			statusIs(t, status, smb.StatusDuplicateObjectID)
			req.GrantedAccess ^= 2
			_, status = table.Replay(req)
			statusIs(t, status, smb.StatusInvalidParameter)
		})
	}
}

func TestMetadataOnlyDoesNotAcquireReadSharing(t *testing.T) {
	table := newTable(t)
	denyRead := request(1)
	denyRead.Sharing = state.ShareMode(state.RightWrite | state.RightDelete)
	commit(t, table, denyRead, state.Grant{})
	metadata := request(1)
	metadata.GrantedAccess = 0x80
	commit(t, table, metadata, state.Grant{})
}

func TestFindAndDirectorySnapshots(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, request(1), state.Grant{})
	if open.ID.Persistent == 0 || open.ID.Volatile == 0 || open.ID.Persistent == ^uint64(0) || open.ID.Volatile == ^uint64(0) {
		t.Fatalf("invalid FileID: %+v", open.ID)
	}
	badID := open.ID
	badID.Volatile++
	_, status := table.Find(badID, binding)
	statusIs(t, status, smb.StatusFileClosed)
	_, status = table.Find(open.ID, state.Binding{SessionID: 2, TreeID: 1})
	statusIs(t, status, smb.StatusFileClosed)
	statusIs(t, table.SetDirectory(open.ID, binding, state.DirectoryCursor{Pattern: "*.band", Cookie: 1}), smb.StatusSuccess)
	statusIs(t, table.SetDirectory(open.ID, binding, state.DirectoryCursor{Cookie: 2}), smb.StatusSuccess)
	found, status := table.Find(open.ID, binding)
	statusIs(t, status, smb.StatusSuccess)
	if found.Directory.Pattern != "*.band" || found.Directory.Cookie != 2 || open.Directory.Pattern != "" {
		t.Fatalf("cursor snapshots: old %+v, new %+v", open.Directory, found.Directory)
	}
	statusIs(t, table.SetDirectory(open.ID, binding, state.DirectoryCursor{Pattern: "*"}), smb.StatusSuccess)
	found, status = table.Find(open.ID, binding)
	statusIs(t, status, smb.StatusSuccess)
	if found.Directory.Cookie != 0 || found.Directory.Pattern != "*" {
		t.Fatalf("restart: %+v", found.Directory)
	}
}

func TestInvalidReservationDoesNotAcquireSharing(t *testing.T) {
	for _, modify := range []func(*state.OpenRequest){
		func(req *state.OpenRequest) { req.Object.Inode = 0 },
		func(req *state.OpenRequest) { req.Binding = state.Binding{} },
		func(req *state.OpenRequest) { req.SharingIntent = 8 },
		func(req *state.OpenRequest) { req.Sharing = 8 },
	} {
		table := newTable(t)
		req := request(1)
		modify(&req)
		_, status := table.Reserve(req)
		statusIs(t, status, smb.StatusInvalidParameter)
		commit(t, table, request(1), state.Grant{})
	}
}
