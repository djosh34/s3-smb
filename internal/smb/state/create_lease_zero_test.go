package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestCreateJoinsHeldHandleOnlyLease(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, 3)
	commit(t, table, req, grant)
	startBreak(t, table, req.Object, smb.LeaseHandle)
	_, _, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, smb.LeaseHandle)
	statusIs(t, status, smb.StatusSuccess)
	writer := request(1)
	writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
	commit(t, table, writer, state.Grant{})
	token, status := table.Reserve(req)
	statusIs(t, status, smb.StatusSuccess)
	// R is not a superset of held H: preserve membership, not promotion.
	joined, status := table.PrepareLease(token, leaseGrant(req, smb.LeaseRead).Lease)
	statusIs(t, status, smb.StatusSuccess)
	if joined.State != 0 || joined.Key != grant.Lease.Key {
		t.Fatalf("held H join = %+v", joined)
	}
	open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: joined})
	statusIs(t, status, smb.StatusSuccess)
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, open.LeaseKey)
	if !exists || live.State != smb.LeaseHandle || live.Breaking {
		t.Fatalf("held H lease was demoted: %+v", live)
	}
}

func TestZeroCachingGrantDoesNotCreateLease(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, 0)
	open := commit(t, table, req, grant)
	if open.LeaseKey != (state.GUID{}) {
		t.Fatal("new zero-state lease was associated")
	}
	if _, exists := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key); exists {
		t.Fatal("new zero-state lease was published")
	}
}

func TestCreateJoinsSharedLeaseBreakingToNone(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	grant.Lease.Epoch, grant.Lease.ParentKey = 7, state.GUID{4}
	commit(t, table, req, grant)
	notification := startBreak(t, table, req.Object, 0)
	token, status := table.Reserve(req)
	statusIs(t, status, smb.StatusSuccess)
	joined, status := table.PrepareLease(token, grant.Lease)
	statusIs(t, status, smb.StatusSuccess)
	if joined.State != 0 || joined.Key != grant.Lease.Key {
		t.Fatalf("pending NONE grant lost its shared key: %+v", joined)
	}
	open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: joined})
	statusIs(t, status, smb.StatusSuccess)
	if open.LeaseKey != grant.Lease.Key {
		t.Fatal("shared open lost the pending lease")
	}
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, open.LeaseKey)
	if !exists || live.State != 7 || !live.Breaking || live.BreakTo != 0 || live.Epoch != notification.Epoch || live.ParentKey != grant.Lease.ParentKey {
		t.Fatalf("pending lease = %+v", live)
	}
	_, _, status = table.AckBreak(binding, req.ClientGUID, open.LeaseKey, 0)
	statusIs(t, status, smb.StatusSuccess)
	live, exists = table.LeaseFor(req.Object, req.ClientGUID, open.LeaseKey)
	if !exists || live.State != 0 || live.Breaking {
		t.Fatalf("completed lease = %+v", live)
	}
}
