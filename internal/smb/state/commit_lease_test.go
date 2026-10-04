package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestCommitLeaseRefreshesPreparedGrantAtomically(t *testing.T) {
	table := newTable(t)
	writer := request(1)
	writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
	mutator := commit(t, table, writer, state.Grant{})
	req := request(1)
	req.CreateGUID, req.GrantedAccess = state.GUID{5}, 1
	wanted := leaseGrant(req, 7).Lease
	wanted.Epoch = 7
	reservation := reserve(t, table, req)
	selected, status := table.PrepareLease(reservation, wanted)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != smb.LeaseRead|smb.LeaseHandle {
		t.Fatalf("initial proposal = %+v", selected)
	}
	mutation, _, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	grant := state.Grant{Handle: &handle{key: req.Object}, Lease: selected, DurableTimeout: smb.DefaultDurableTimeout, CreateAction: 1, WriteThrough: true}
	open, status := table.CommitLease(reservation, grant, wanted)
	statusIs(t, status, smb.StatusSuccess)
	if open.LeaseKey != (state.GUID{}) || open.Durable || !open.WriteThrough || open.CreateAction != 1 || open.Handle != grant.Handle {
		t.Fatalf("atomic commit exposed stale rights or lost storage/mode fields: %+v", open)
	}
	fresh, lease, status := table.LeaseForOpen(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if !fresh.WriteThrough || fresh.Durable || lease != (state.Lease{}) {
		t.Fatalf("fresh snapshot lost mode or promised stale caching: %+v, %+v", fresh, lease)
	}
	if _, exists := table.LeaseFor(req.Object, req.ClientGUID, wanted.Key); exists {
		t.Fatal("declined atomic grant published an acquisition epoch")
	}
	statusIs(t, table.Abort(reservation), smb.StatusInvalidParameter)
}

func TestCommitLeaseDoesNotNormalizeInvalidGrantMetadata(t *testing.T) {
	for _, test := range []struct {
		change func(*state.Grant, *state.Lease)
		name   string
	}{
		{name: "nil handle", change: func(grant *state.Grant, _ *state.Lease) { grant.Handle = nil }},
		{name: "negative timeout", change: func(grant *state.Grant, _ *state.Lease) { grant.DurableTimeout = -1 }},
		{name: "over-cap timeout", change: func(grant *state.Grant, _ *state.Lease) { grant.DurableTimeout = smb.MaxDurableTimeout + 1 }},
		{name: "invalid selected state", change: func(grant *state.Grant, _ *state.Lease) { grant.Lease.State = smb.LeaseWrite }},
		{name: "breaking selected grant", change: func(grant *state.Grant, _ *state.Lease) { grant.Lease.Breaking = true }},
		{name: "wrong selected client", change: func(grant *state.Grant, _ *state.Lease) { grant.Lease.ClientGUID = state.GUID{8} }},
		{name: "wrong selected key", change: func(grant *state.Grant, _ *state.Lease) { grant.Lease.Key = state.GUID{8} }},
		{name: "zero selected key", change: func(grant *state.Grant, _ *state.Lease) { grant.Lease.Key = state.GUID{} }},
		{name: "selected directory lease", change: func(grant *state.Grant, _ *state.Lease) { grant.Directory = true }},
		{name: "invalid original request", change: func(_ *state.Grant, wanted *state.Lease) { wanted.State = smb.LeaseWrite }},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			writer := request(1)
			writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
			mutator := commit(t, table, writer, state.Grant{})
			req := request(1)
			req.CreateGUID = state.GUID{5}
			wanted := leaseGrant(req, 7).Lease
			reservation := reserve(t, table, req)
			selected, status := table.PrepareLease(reservation, wanted)
			statusIs(t, status, smb.StatusSuccess)
			grant := state.Grant{Handle: &handle{key: req.Object}, Lease: selected, DurableTimeout: smb.DefaultDurableTimeout}
			test.change(&grant, &wanted)
			mutation, _, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
			statusIs(t, status, smb.StatusSuccess)
			defer table.EndMutation(mutation)
			_, status = table.CommitLease(reservation, grant, wanted)
			statusIs(t, status, smb.StatusInvalidParameter)
			statusIs(t, table.Abort(reservation), smb.StatusSuccess)
		})
	}
}
