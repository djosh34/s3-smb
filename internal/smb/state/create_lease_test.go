package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestPrepareLeaseSafeSubsets(t *testing.T) {
	for _, test := range []struct {
		name      string
		other     bool
		writer    bool
		reserved  bool
		requested uint32
		want      uint32
	}{
		{name: "R", requested: smb.LeaseRead, want: smb.LeaseRead},
		{name: "RH", requested: smb.LeaseRead | smb.LeaseHandle, want: smb.LeaseRead | smb.LeaseHandle},
		{name: "RWH", requested: 7, want: 7},
		{name: "reader removes W", other: true, requested: 7, want: 3},
		{name: "writer removes caching", other: true, writer: true, requested: 7},
		{name: "reservation removes W", other: true, reserved: true, requested: 7, want: 3},
		{name: "writer reservation removes caching", other: true, writer: true, reserved: true, requested: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			if test.other {
				other := request(1)
				other.ClientGUID = state.GUID{9}
				other.GrantedAccess = 1
				if test.writer {
					other.GrantedAccess = 2
				}
				if test.reserved {
					token, status := table.Reserve(other)
					statusIs(t, status, smb.StatusSuccess)
					defer func() { statusIs(t, table.Abort(token), smb.StatusSuccess) }()
				} else {
					commit(t, table, other, state.Grant{})
				}
			}
			req := request(1)
			token, status := table.Reserve(req)
			statusIs(t, status, smb.StatusSuccess)
			requested := leaseGrant(req, test.requested).Lease
			requested.Epoch, requested.ParentKey = 9, state.GUID{4}
			grant, status := table.PrepareLease(token, requested)
			statusIs(t, status, smb.StatusSuccess)
			if grant.State != test.want {
				t.Fatalf("grant = %+v, want %#x", grant, test.want)
			}
			if _, exists := table.LeaseFor(req.Object, req.ClientGUID, requested.Key); exists {
				t.Fatal("PrepareLease published a lease")
			}
			_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: grant})
			statusIs(t, status, smb.StatusSuccess)
			if test.want != 0 {
				live, exists := table.LeaseFor(req.Object, req.ClientGUID, requested.Key)
				if !exists || live.State != test.want || live.Epoch != 10 || live.ParentKey != requested.ParentKey {
					t.Fatalf("committed lease = %+v, exists %v", live, exists)
				}
			}
		})
	}
}

func TestPrepareLeaseSharedStateAndEpoch(t *testing.T) {
	table := newTable(t)
	req := request(1)
	first := leaseGrant(req, smb.LeaseRead)
	first.Lease.ParentKey, first.Lease.Epoch = state.GUID{4}, 7
	commit(t, table, req, first)
	for _, test := range []struct {
		requested, want uint32
		epoch           uint16
	}{
		{requested: 3, want: 3, epoch: 8},
		{requested: 7, want: 7, epoch: 9},
		{requested: 1, want: 7, epoch: 9},
	} {
		token, status := table.Reserve(req)
		statusIs(t, status, smb.StatusSuccess)
		wanted := first.Lease
		wanted.State, wanted.Epoch, wanted.ParentKey = test.requested, 999, state.GUID{8}
		grant, status := table.PrepareLease(token, wanted)
		statusIs(t, status, smb.StatusSuccess)
		_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: grant})
		statusIs(t, status, smb.StatusSuccess)
		live, exists := table.LeaseFor(req.Object, req.ClientGUID, first.Lease.Key)
		if !exists || live.State != test.want || live.Epoch != test.epoch || live.ParentKey != first.Lease.ParentKey {
			t.Fatalf("shared lease = %+v, exists %v", live, exists)
		}
		live.State, live.Epoch = 0, 0
		copy, _ := table.LeaseFor(req.Object, req.ClientGUID, first.Lease.Key)
		if copy.State != test.want || copy.Epoch != test.epoch {
			t.Fatal("LeaseFor returned mutable table state")
		}
	}
	startBreak(t, table, req.Object, smb.LeaseRead)
	token, status := table.Reserve(req)
	statusIs(t, status, smb.StatusSuccess)
	grant, status := table.PrepareLease(token, leaseGrant(req, 7).Lease)
	statusIs(t, status, smb.StatusSuccess)
	if grant.State != smb.LeaseRead || grant.Epoch != 10 {
		t.Fatalf("pending shared grant = %+v", grant)
	}
	_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: grant})
	statusIs(t, status, smb.StatusSuccess)
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, first.Lease.Key)
	if !exists || !live.Breaking || live.State != 7 || live.BreakTo != 1 || live.Epoch != 10 {
		t.Fatalf("pending lease = %+v", live)
	}
	if _, exists = table.LeaseFor(smb.ObjectKey{Inode: 2}, req.ClientGUID, first.Lease.Key); exists {
		t.Fatal("LeaseFor crossed objects")
	}
}

func TestPrepareLeaseRejectsInvalidIdentity(t *testing.T) {
	table := newTable(t)
	req := request(1)
	lease := leaseGrant(req, 7).Lease
	commit(t, table, req, state.Grant{Lease: lease})
	other := request(2)
	token, status := table.Reserve(other)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.PrepareLease(token, lease)
	statusIs(t, status, smb.StatusInvalidParameter)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	_, status = table.PrepareLease(token, lease)
	statusIs(t, status, smb.StatusInvalidParameter)
}

func TestSharingHandleLeaseQuery(t *testing.T) {
	for _, held := range []uint32{0, smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle} {
		table := newTable(t)
		req := request(1)
		req.GrantedAccess, req.Sharing = 1, state.ShareMode(state.RightRead)
		commit(t, table, req, leaseGrant(req, held))
		other := request(1)
		other.ClientGUID, other.GrantedAccess = state.GUID{9}, 2
		_, status := table.Reserve(other)
		statusIs(t, status, smb.StatusSharingViolation)
		objects := table.SharingHandleLeases(other)
		if held&smb.LeaseHandle == 0 && len(objects) != 0 || held&smb.LeaseHandle != 0 && (len(objects) != 1 || objects[0] != req.Object) {
			t.Fatalf("held %#x, conflicting H objects = %+v", held, objects)
		}
		other.GrantedAccess = 1
		if objects = table.SharingHandleLeases(other); len(objects) != 0 {
			t.Fatalf("compatible open breaks unrelated H: %+v", objects)
		}
	}
}

func TestHandleOnlyBreakRetainsDurability(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State = 7
	open := commit(t, table, req, grant)
	notification := startBreak(t, table, req.Object, smb.LeaseHandle)
	if notification.NewState != smb.LeaseHandle || !notification.AckRequired {
		t.Fatalf("H break = %+v", notification)
	}
	_, status := table.AckBreak(binding, req.ClientGUID, open.LeaseKey, smb.LeaseHandle)
	statusIs(t, status, smb.StatusSuccess)
	found, status := table.Find(open.ID, binding)
	statusIs(t, status, smb.StatusSuccess)
	if !found.Durable {
		t.Fatal("writer break dropped durability")
	}
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, open.LeaseKey)
	if !exists || live.State != smb.LeaseHandle || live.Breaking {
		t.Fatalf("H lease = %+v", live)
	}
}
