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

// A share mode that leaves out a right conflicts with an open using it,
// whichever comes first and whether or not the first is committed yet.
func TestShareChecksBothDirections(t *testing.T) {
	for _, rights := range []state.Rights{state.RightRead, state.RightWrite, state.RightDelete} {
		for _, committed := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				table := newTable(t)
				deny, need := request(1), request(1)
				deny.Sharing = state.ShareMode(state.Rights(shareAll) &^ rights)
				deny.SharingIntent, need.SharingIntent = rights, rights
				first, second := deny, need
				if reverse {
					first, second = need, deny
				}
				if committed {
					commit(t, table, first, state.Grant{})
				} else {
					reserve(t, table, first)
				}
				if _, status := table.Reserve(second); status != smb.StatusSharingViolation {
					t.Fatalf("rights %d, committed %t, reverse %t: status %#x", rights, committed, reverse, status)
				}
			}
		}
	}
}

// A reservation holds its share mode until it is committed or aborted, and
// can be used only once.
func TestReservationLifecycle(t *testing.T) {
	table := newTable(t)
	writer := request(1)
	writer.SharingIntent, writer.Sharing = state.RightWrite, 0
	token := reserve(t, table, writer)
	_, status := table.Commit(token, state.Grant{})
	statusIs(t, status, smb.StatusInvalidParameter)
	reader := request(1)
	reader.SharingIntent = state.RightRead
	_, status = table.Reserve(reader)
	statusIs(t, status, smb.StatusSharingViolation)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	statusIs(t, table.Abort(token), smb.StatusInvalidParameter)
	token = reserve(t, table, writer)
	grant := state.Grant{Handle: &handle{key: writer.Object}}
	open, status := table.Commit(token, grant)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.Commit(token, grant)
	statusIs(t, status, smb.StatusInvalidParameter)
	statusIs(t, table.Abort(token), smb.StatusInvalidParameter)
	closeOpen(t, table, open)
	_, status = table.Commit(0, grant)
	statusIs(t, status, smb.StatusInvalidParameter)
}

// Only the binding that holds an open can use it. After a reconnect the old
// binding can change nothing.
func TestOpenBelongsToItsBinding(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	req.GrantedAccess |= 0x10000
	open := commit(t, table, req, durableGrant(req))
	unknown := open.ID
	unknown.Volatile++
	_, status := table.Find(unknown, binding)
	statusIs(t, status, smb.StatusFileClosed)
	table.Disconnect(binding.SessionID)
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
		t.Fatal("the old binding changed the reconnected open")
	}
}

// A replayed durable CREATE finds its open only with the same user, share,
// client and create GUID, and not while the first CREATE is still pending.
func TestLookupCreateMatchesTheWholeIdentity(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	_, status := table.LookupCreate(req)
	statusIs(t, status, smb.StatusObjectNameNotFound)
	token := reserve(t, table, req)
	_, status = table.LookupCreate(req)
	statusIs(t, status, smb.StatusDuplicateObjectID)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	open := commit(t, table, req, durableGrant(req))
	found, status := table.LookupCreate(req)
	statusIs(t, status, smb.StatusSuccess)
	if found != open {
		t.Fatalf("lookup = %+v, want %+v", found, open)
	}
	for _, modify := range []func(*state.OpenRequest){
		func(r *state.OpenRequest) { r.User += "other" },
		func(r *state.OpenRequest) { r.Share += "other" },
		func(r *state.OpenRequest) { r.ClientGUID[0]++ },
		func(r *state.OpenRequest) { r.CreateGUID[0]++ },
	} {
		other := req
		modify(&other)
		_, status = table.LookupCreate(other)
		statusIs(t, status, smb.StatusObjectNameNotFound)
	}
	closeOpen(t, table, open)
	_, status = table.LookupCreate(req)
	statusIs(t, status, smb.StatusObjectNameNotFound)
}
