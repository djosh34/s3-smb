package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestLeaseForOpenReturnsFreshImmutableSnapshot(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	open := commit(t, table, req, grant)
	fresh, lease, status := table.LeaseForOpen(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if !fresh.Durable || fresh.DurableTimeout != smb.DefaultDurableTimeout || lease.State != smb.LeaseRead|smb.LeaseHandle {
		t.Fatalf("initial snapshot = %+v, %+v", fresh, lease)
	}
	fresh.Durable, lease.State = false, 0
	fresh, lease, status = table.LeaseForOpen(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if !fresh.Durable || lease.State != smb.LeaseRead|smb.LeaseHandle {
		t.Fatal("snapshot exposed mutable table state")
	}
	startBreak(t, table, req.Object, smb.LeaseRead)
	fresh, lease, status = table.LeaseForOpen(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if !fresh.Durable || !lease.Breaking || lease.State != smb.LeaseRead|smb.LeaseHandle || lease.BreakTo != smb.LeaseRead {
		t.Fatalf("pending snapshot = %+v, %+v", fresh, lease)
	}
	_, _, status = table.AckBreak(open.Binding, req.ClientGUID, open.LeaseKey, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
	fresh, lease, status = table.LeaseForOpen(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if fresh.Durable || lease.Breaking || lease.State != smb.LeaseRead {
		t.Fatalf("snapshot retained old durability/state = %+v, %+v", fresh, lease)
	}
	wrong := open.ID
	wrong.Volatile++
	_, _, status = table.LeaseForOpen(wrong, open.Binding)
	statusIs(t, status, smb.StatusFileClosed)
	_, _, status = table.LeaseForOpen(open.ID, state.Binding{SessionID: 9, TreeID: 1})
	statusIs(t, status, smb.StatusFileClosed)
}

func TestMetadataOnlyOpenCannotAcquireCachingBesideWriteLease(t *testing.T) {
	table := newTable(t)
	req := request(1)
	initial := leaseGrant(req, 7)
	commit(t, table, req, initial)
	metadata := req
	metadata.ClientGUID, metadata.GrantedAccess = state.GUID{9}, 0x120180
	token := reserve(t, table, metadata)
	selected, status := table.PrepareLease(token, leaseGrant(metadata, 7).Lease)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != 0 {
		t.Fatalf("metadata OPEN acquired caching beside competing W: %+v", selected)
	}
	open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
	statusIs(t, status, smb.StatusSuccess)
	if open.LeaseKey != (state.GUID{}) {
		t.Fatalf("unsafe new lease was associated: %+v", open)
	}
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, initial.Lease.Key)
	if !exists || live.State != 7 || live.Breaking {
		t.Fatalf("metadata OPEN changed held W: %+v, exists %v", live, exists)
	}
}

func TestPreparedMembershipDoesNotReacquireAfterBreak(t *testing.T) {
	table := newTable(t)
	req := request(1)
	initial := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle)
	initial.Lease.Epoch = 8
	commit(t, table, req, initial)
	token := reserve(t, table, req)
	wanted := initial.Lease
	wanted.State, wanted.Epoch = smb.LeaseRead, 999
	selected, status := table.PrepareLease(token, wanted)
	statusIs(t, status, smb.StatusSuccess)
	startBreak(t, table, req.Object, smb.LeaseRead)
	_, _, status = table.AckBreak(binding, req.ClientGUID, initial.Lease.Key, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
	open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
	statusIs(t, status, smb.StatusSuccess)
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, open.LeaseKey)
	if !exists || live.State != smb.LeaseRead || live.Epoch != 9 {
		t.Fatalf("membership reacquired rights after break: %+v, exists %v", live, exists)
	}
}

func TestLeaseAcquisitionAndPromotionEpochsPublishOnlyOnCommit(t *testing.T) {
	table := newTable(t)
	req := request(1)
	wanted := leaseGrant(req, smb.LeaseRead).Lease
	wanted.Epoch = 0xffff
	token := reserve(t, table, req)
	selected, status := table.PrepareLease(token, wanted)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != smb.LeaseRead || selected.Epoch != 0 {
		t.Fatalf("prepared acquisition did not wrap once: %+v", selected)
	}
	if _, exists := table.LeaseFor(req.Object, req.ClientGUID, wanted.Key); exists {
		t.Fatal("preparation published the acquisition")
	}
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	token = reserve(t, table, req)
	selected, status = table.PrepareLease(token, wanted)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
	statusIs(t, status, smb.StatusSuccess)
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, wanted.Key)
	if !exists || live.State != smb.LeaseRead || live.Epoch != 0 {
		t.Fatalf("acquisition did not wrap once: %+v, exists %v", live, exists)
	}

	wanted.State, wanted.Epoch = smb.LeaseRead|smb.LeaseHandle, 999
	token = reserve(t, table, req)
	selected, status = table.PrepareLease(token, wanted)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.Commit(token, state.Grant{Lease: selected})
	statusIs(t, status, smb.StatusInvalidParameter)
	live, _ = table.LeaseFor(req.Object, req.ClientGUID, wanted.Key)
	if live.State != smb.LeaseRead || live.Epoch != 0 {
		t.Fatalf("failed commit advanced promotion: %+v", live)
	}
	_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
	statusIs(t, status, smb.StatusSuccess)
	live, _ = table.LeaseFor(req.Object, req.ClientGUID, wanted.Key)
	if live.State != smb.LeaseRead|smb.LeaseHandle || live.Epoch != 1 {
		t.Fatalf("promotion did not advance shared epoch once: %+v", live)
	}
}

func TestSameKeyPendingOpenKeepsCapturedLease(t *testing.T) {
	for _, target := range []uint32{0, smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle} {
		table := newTable(t)
		req := request(1)
		initial := leaseGrant(req, 7)
		initial.Lease.Epoch, initial.Lease.ParentKey = 8, state.GUID{4}
		commit(t, table, req, initial)
		startBreak(t, table, req.Object, target)
		captured, _ := table.LeaseFor(req.Object, req.ClientGUID, initial.Lease.Key)
		token := reserve(t, table, req)
		wanted := initial.Lease
		wanted.Epoch, wanted.ParentKey = 999, state.GUID{8}
		selected, status := table.PrepareLease(token, wanted)
		statusIs(t, status, smb.StatusSuccess)
		if selected.State != 0 || selected.Key != initial.Lease.Key || selected.Epoch != 9 {
			t.Fatalf("pending join = %+v, want membership without acquisition", selected)
		}
		if table.DurableEligible(token, selected) != (target == smb.LeaseRead|smb.LeaseHandle) {
			t.Fatalf("durability did not follow effective pending H for target %#x", target)
		}
		open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
		statusIs(t, status, smb.StatusSuccess)
		if open.LeaseKey != initial.Lease.Key {
			t.Fatalf("pending join lost membership: %+v", open)
		}
		live, exists := table.LeaseFor(req.Object, req.ClientGUID, initial.Lease.Key)
		if !exists || live != captured {
			t.Fatalf("pending join changed captured lease: %+v, want %+v", live, captured)
		}
	}
}

func TestMetadataOnlyOpenPreservesCompetingWriteLease(t *testing.T) {
	for _, access := range []uint32{0, 0x80, 0x100, 0x20000, 0x100000, 0x120180} {
		table := newTable(t)
		req := request(1)
		initial := leaseGrant(req, 7)
		initial.Lease.Epoch = 8
		commit(t, table, req, initial)
		metadata := req
		metadata.ClientGUID, metadata.GrantedAccess = state.GUID{9}, access
		token := reserve(t, table, metadata)
		_, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}})
		statusIs(t, status, smb.StatusSuccess)
		live, exists := table.LeaseFor(req.Object, req.ClientGUID, initial.Lease.Key)
		if !exists || live.State != 7 || live.Epoch != 8 || live.Breaking {
			t.Fatalf("metadata access %#x changed lease: %+v, exists %v", access, live, exists)
		}
	}
}

func TestSameKeyOpenPreservesLeaseAndEpoch(t *testing.T) {
	for _, requestedState := range []uint32{0, smb.LeaseRead, 7} {
		t.Run(map[uint32]string{0: "none", 1: "smaller", 7: "unchanged"}[requestedState], func(t *testing.T) {
			table := newTable(t)
			req := request(1)
			initial := leaseGrant(req, 7).Lease
			initial.Epoch, initial.ParentKey = 0x4711, state.GUID{4}
			token := reserve(t, table, req)
			selected, status := table.PrepareLease(token, initial)
			statusIs(t, status, smb.StatusSuccess)
			_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
			statusIs(t, status, smb.StatusSuccess)

			writer := req
			writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
			waiting := reserve(t, table, writer)
			t.Cleanup(func() { statusIs(t, table.Abort(waiting), smb.StatusSuccess) })
			token = reserve(t, table, req)
			wanted := initial
			wanted.State, wanted.Epoch, wanted.ParentKey = requestedState, 999, state.GUID{8}
			selected, status = table.PrepareLease(token, wanted)
			statusIs(t, status, smb.StatusSuccess)
			open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
			statusIs(t, status, smb.StatusSuccess)
			if open.LeaseKey != initial.Key {
				t.Fatalf("reopen lost shared membership: %+v", open)
			}
			live, exists := table.LeaseFor(req.Object, req.ClientGUID, initial.Key)
			if !exists || live.State != 7 || live.Epoch != 0x4712 || live.ParentKey != initial.ParentKey {
				t.Fatalf("reopen changed shared lease: %+v, exists %v", live, exists)
			}
		})
	}
}

// Ordinary writable opens do not invalidate R/H; the actual mutation does.
// MS-SMB2 3.3.1.4 and MS-FSA 2.1.4.12.
func TestLeaseGrantWithOtherWriterKeepsRH(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "reservation"}[reserved], func(t *testing.T) {
			table := newTable(t)
			writer := request(1)
			writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
			if reserved {
				token := reserve(t, table, writer)
				t.Cleanup(func() { statusIs(t, table.Abort(token), smb.StatusSuccess) })
			} else {
				commit(t, table, writer, state.Grant{})
			}
			req := request(1)
			token := reserve(t, table, req)
			requested := leaseGrant(req, 7).Lease
			requested.Epoch = 0x4711
			selected, status := table.PrepareLease(token, requested)
			statusIs(t, status, smb.StatusSuccess)
			if selected.State != smb.LeaseRead|smb.LeaseHandle {
				t.Fatalf("selected state = %#x, want RH", selected.State)
			}
			open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
			statusIs(t, status, smb.StatusSuccess)
			live, exists := table.LeaseFor(req.Object, req.ClientGUID, open.LeaseKey)
			if !exists || live.State != smb.LeaseRead|smb.LeaseHandle || live.Epoch != 0x4712 {
				t.Fatalf("published lease = %+v, exists %v", live, exists)
			}
		})
	}
}
