package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestCreateLeaseBreakQueryIncludesPendingBreaks(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	if table.LeasesNeedBreak(req.Object, state.GUID{9}, state.GUID{9}, 3) {
		t.Fatal("missing object needs a break")
	}
	commit(t, table, req, grant)
	if !table.LeasesNeedBreak(req.Object, state.GUID{9}, state.GUID{9}, 3) {
		t.Fatal("W was not detected")
	}
	if table.LeasesNeedBreak(req.Object, req.ClientGUID, grant.Lease.Key, 0) {
		t.Fatal("the requesting lease needs a break")
	}
	startBreak(t, table, req.Object, 3)
	if !table.LeasesNeedBreak(req.Object, state.GUID{9}, state.GUID{9}, 7) {
		t.Fatal("earlier pending break was ignored")
	}
	_, _, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, 3)
	statusIs(t, status, smb.StatusSuccess)
	if table.LeasesNeedBreak(req.Object, state.GUID{9}, state.GUID{9}, 3) {
		t.Fatal("compatible completed lease needs another break")
	}
	if !table.LeasesNeedBreak(req.Object, state.GUID{9}, state.GUID{9}, smb.LeaseHandle) {
		t.Fatal("writer open did not detect R")
	}
}
