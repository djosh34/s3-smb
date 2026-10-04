package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestLookupCreateIdentityAndReservation(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	_, status := table.LookupCreate(req)
	statusIs(t, status, smb.StatusObjectNameNotFound)
	token := reserve(t, table, req)
	_, status = table.LookupCreate(req)
	statusIs(t, status, smb.StatusDuplicateObjectID)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	grant := durableGrant(req)
	grant.CreateAction = 2
	open := commit(t, table, req, grant)
	found, status := table.LookupCreate(req)
	statusIs(t, status, smb.StatusSuccess)
	if found != open || found.CreateAction != 2 {
		t.Fatalf("lookup changed original open: %+v", found)
	}
	for _, modify := range []func(*state.OpenRequest){
		func(r *state.OpenRequest) { r.User += "other" },
		func(r *state.OpenRequest) { r.Share += "other" },
		func(r *state.OpenRequest) { r.ClientGUID[0]++ },
		func(r *state.OpenRequest) { r.CreateGUID[0]++ },
	} {
		bad := req
		modify(&bad)
		_, status = table.LookupCreate(bad)
		statusIs(t, status, smb.StatusObjectNameNotFound)
	}
	bad := req
	bad.CreateParameters[0]++
	_, status = table.LookupCreate(bad)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.Replay(bad)
	statusIs(t, status, smb.StatusInvalidParameter)
	closeOpen(t, table, open)
	_, status = table.LookupCreate(req)
	statusIs(t, status, smb.StatusObjectNameNotFound)
}
