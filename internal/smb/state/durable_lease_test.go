package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func heldHandleJoin(t *testing.T) (*state.Table, state.OpenRequest, state.Reservation, state.Lease) {
	t.Helper()
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle)
	commit(t, table, req, grant)
	startBreak(t, table, req.Object, smb.LeaseHandle)
	_, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, smb.LeaseHandle)
	statusIs(t, status, smb.StatusSuccess)
	writer := request(1)
	writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
	commit(t, table, writer, state.Grant{})
	req.CreateGUID = state.GUID{5}
	token := reserve(t, table, req)
	joined, status := table.PrepareLease(token, leaseGrant(req, 7).Lease)
	statusIs(t, status, smb.StatusSuccess)
	if joined.State != 0 || joined.Key != grant.Lease.Key {
		t.Fatalf("held H selection = %+v", joined)
	}
	return table, req, token, joined
}

func TestDurableJoinUsesEffectiveSharedH(t *testing.T) {
	table, req, token, joined := heldHandleJoin(t)
	if !table.DurableEligible(token, joined) {
		t.Fatal("held shared H did not permit durability")
	}
	open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: joined, DurableTimeout: smb.DefaultDurableTimeout})
	statusIs(t, status, smb.StatusSuccess)
	if !open.Durable || open.LeaseKey != joined.Key {
		t.Fatalf("held H durable join = %+v", open)
	}
	if table.DurableEligible(token, joined) {
		t.Fatal("consumed reservation still eligible")
	}
}

func TestDurableJoinDeniesPendingNone(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	commit(t, table, req, grant)
	startBreak(t, table, req.Object, 0)
	req.CreateGUID = state.GUID{5}
	token := reserve(t, table, req)
	joined, status := table.PrepareLease(token, grant.Lease)
	statusIs(t, status, smb.StatusSuccess)
	if joined.State != 0 || joined.Key != grant.Lease.Key || table.DurableEligible(token, joined) {
		t.Fatalf("pending NONE eligibility = %+v", joined)
	}
	proposed := state.Grant{Handle: &handle{key: req.Object}, Lease: joined, DurableTimeout: smb.DefaultDurableTimeout}
	_, status = table.Commit(token, proposed)
	statusIs(t, status, smb.StatusInvalidParameter)
	proposed.DurableTimeout = 0
	open, status := table.Commit(token, proposed)
	statusIs(t, status, smb.StatusSuccess)
	if open.Durable || open.LeaseKey != joined.Key {
		t.Fatalf("pending NONE join = %+v", open)
	}
}

func TestDurableCommitRechecksBreakAfterEligibilitySnapshot(t *testing.T) {
	table, req, token, joined := heldHandleJoin(t)
	if !table.DurableEligible(token, joined) {
		t.Fatal("initial held H eligibility failed")
	}
	// An H-removing break starts after the handler's snapshot but before its
	// commit. Zero selected rights still join the lease, not its durability.
	startBreak(t, table, req.Object, 0)
	_, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: joined, DurableTimeout: smb.DefaultDurableTimeout})
	statusIs(t, status, smb.StatusInvalidParameter)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
}

func TestDurableJoinAllowsBreakRetainingH(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	commit(t, table, req, grant)
	startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	req.CreateGUID = state.GUID{5}
	token := reserve(t, table, req)
	joined, status := table.PrepareLease(token, grant.Lease)
	statusIs(t, status, smb.StatusSuccess)
	if !table.DurableEligible(token, joined) {
		t.Fatal("H-retaining break refused durability")
	}
	open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: joined, DurableTimeout: smb.DefaultDurableTimeout})
	statusIs(t, status, smb.StatusSuccess)
	if !open.Durable {
		t.Fatal("H-retaining join did not become durable")
	}
}
