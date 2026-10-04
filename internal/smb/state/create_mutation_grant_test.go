package state_test

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestMutationGrantExemptionUsesExactClientAndKey(t *testing.T) {
	table := newTable(t)
	req := request(1)
	req.GrantedAccess = 1
	initial := leaseGrant(req, smb.LeaseRead)
	initial.Lease.Epoch = 8
	mutator := commit(t, table, req, initial)
	mutation, notifications, actions, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	if len(notifications) != 0 || len(actions) != 0 {
		t.Fatal("own lease mutation changed held rights")
	}
	for _, test := range []struct {
		name   string
		client state.GUID
		key    state.GUID
		want   uint32
	}{
		{name: "same client different key", client: req.ClientGUID, key: state.GUID{9}},
		{name: "different client same key", client: state.GUID{9}, key: initial.Lease.Key},
		{name: "same identity promotion", client: req.ClientGUID, key: initial.Lease.Key, want: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			other := req
			other.ClientGUID = test.client
			token := reserve(t, table, other)
			wanted := initial.Lease
			wanted.ClientGUID, wanted.Key, wanted.State = test.client, test.key, 7
			selected, selectedStatus := table.PrepareLease(token, wanted)
			statusIs(t, selectedStatus, smb.StatusSuccess)
			if selected.State != test.want {
				t.Fatalf("mutation identity selected %+v, want %#x", selected, test.want)
			}
			if test.want == 0 {
				statusIs(t, table.Abort(token), smb.StatusSuccess)
				return
			}
			_, committedStatus := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
			statusIs(t, committedStatus, smb.StatusSuccess)
			live, exists := table.LeaseFor(req.Object, req.ClientGUID, initial.Lease.Key)
			if !exists || live.State != 7 || live.Epoch != 9 {
				t.Fatalf("same identity promotion = %+v, exists %v", live, exists)
			}
		})
	}
}

func TestMutationWithholdsReacquisitionWithoutChangingMembership(t *testing.T) {
	table := newTable(t)
	req := request(1)
	initial := leaseGrant(req, smb.LeaseRead)
	initial.Lease.Epoch = 8
	commit(t, table, req, initial)
	writer := req
	writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
	mutator := commit(t, table, writer, state.Grant{})
	mutation, notifications, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	if len(notifications) != 1 || notifications[0].NewState != 0 || notifications[0].AckRequired {
		t.Fatalf("R-only invalidation = %+v", notifications)
	}
	token := reserve(t, table, req)
	wanted := initial.Lease
	wanted.State, wanted.Epoch = 7, 999
	selected, status := table.PrepareLease(token, wanted)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != 0 || selected.Key != initial.Lease.Key || selected.Epoch != 9 {
		t.Fatalf("active mutation reacquired rights or lost membership: %+v", selected)
	}
	joined, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
	statusIs(t, status, smb.StatusSuccess)
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, joined.LeaseKey)
	if !exists || live.State != 0 || live.Epoch != 9 || live.Breaking {
		t.Fatalf("membership changed the invalidated lease: %+v, exists %v", live, exists)
	}
}

func TestPreparedCommitAndMutationLinearizeGrantPublication(t *testing.T) {
	for range 50 {
		checkPreparedCommitAndMutationLinearizeGrantPublication(t)
	}
}

func checkPreparedCommitAndMutationLinearizeGrantPublication(t *testing.T) {
	t.Helper()
	table := newTable(t)
	writer := request(1)
	writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
	mutator := commit(t, table, writer, state.Grant{})
	req := request(1)
	req.GrantedAccess = 1
	wanted := leaseGrant(req, smb.LeaseRead).Lease
	wanted.Epoch = 7
	reservation := reserve(t, table, req)
	selected, status := table.PrepareLease(reservation, wanted)
	statusIs(t, status, smb.StatusSuccess)

	start, began, committed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mutation state.Mutation
	var notifications []state.Break
	var actions []state.CloseAction
	var beginStatus, commitStatus smb.Status
	go func() {
		<-start
		mutation, notifications, actions, beginStatus = table.BeginMutation(mutator.ID, mutator.Binding)
		close(began)
	}()
	go func() {
		<-start
		_, commitStatus = table.Commit(reservation, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
		close(committed)
	}()
	close(start)
	<-began
	<-committed
	statusIs(t, beginStatus, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	if len(actions) != 0 {
		t.Fatalf("R-only race produced cleanup: %+v", actions)
	}
	live, exists := table.LeaseFor(req.Object, req.ClientGUID, wanted.Key)
	if commitStatus != smb.StatusSuccess && commitStatus != smb.StatusInvalidParameter {
		t.Fatalf("unexpected raced commit status %#x", commitStatus)
	}
	if commitStatus == smb.StatusSuccess {
		// Commit won the lock: BeginMutation must capture/revoke that R
		// before the mutator can perform any actual operation.
		if !exists || live.State != 0 || live.Epoch != 9 || len(notifications) != 1 || notifications[0].CurrentState != smb.LeaseRead || notifications[0].NewState != 0 || notifications[0].AckRequired {
			t.Fatalf("commit-first race left unsafe caching: %+v, %+v", live, notifications)
		}
		return
	}
	// BeginMutation won: atomic validation must reject publication.
	if exists || len(notifications) != 0 {
		t.Fatalf("mutation-first race published caching: %+v, %+v", live, notifications)
	}
	statusIs(t, table.Abort(reservation), smb.StatusSuccess)
}

func TestPrepareLeaseMutationGateSurvivesCloseUntilLastTokenEnds(t *testing.T) {
	table := newTable(t)
	writer := request(1)
	writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
	first := commit(t, table, writer, state.Grant{})
	second := commit(t, table, writer, state.Grant{})
	one, _, _, status := table.BeginMutation(first.ID, first.Binding)
	statusIs(t, status, smb.StatusSuccess)
	two, _, _, status := table.BeginMutation(second.ID, second.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(one)
	defer table.EndMutation(two)
	_, status = table.Close(first.ID, first.Binding)
	statusIs(t, status, smb.StatusSuccess)
	checkMutationReadSelection(t, table, request(1), 0)
	table.EndMutation(one)
	table.EndMutation(one)
	_, status = table.Close(second.ID, second.Binding)
	statusIs(t, status, smb.StatusSuccess)
	checkMutationReadSelection(t, table, request(1), 0)
	table.EndMutation(two)
	checkMutationReadSelection(t, table, request(1), smb.LeaseRead)
}

func TestPrepareLeaseMutationGateDoesNotCrossSelectedStream(t *testing.T) {
	table := newTable(t)
	writer := request(1)
	writer.Object.Stream, writer.ClientGUID, writer.GrantedAccess = "resource", state.GUID{9}, 2
	mutator := commit(t, table, writer, state.Grant{})
	mutation, _, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	checkMutationReadSelection(t, table, request(1), smb.LeaseRead)
}

func checkMutationReadSelection(t *testing.T, table *state.Table, req state.OpenRequest, want uint32) {
	t.Helper()
	reservation := reserve(t, table, req)
	selected, status := table.PrepareLease(reservation, leaseGrant(req, smb.LeaseRead).Lease)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != want {
		t.Fatalf("mutation lifetime/object isolation selected %+v, want %#x", selected, want)
	}
	statusIs(t, table.Abort(reservation), smb.StatusSuccess)
}

func TestCommitRejectsGrantPreparedBeforeMutation(t *testing.T) {
	table := newTable(t)
	writer := request(1)
	writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
	mutator := commit(t, table, writer, state.Grant{})
	req := request(1)
	req.GrantedAccess = 1
	wanted := leaseGrant(req, smb.LeaseRead).Lease
	wanted.Epoch = 7
	token := reserve(t, table, req)
	selected, status := table.PrepareLease(token, wanted)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != smb.LeaseRead {
		t.Fatalf("initial proposal = %+v", selected)
	}
	mutation, _, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
	statusIs(t, status, smb.StatusInvalidParameter)
	if _, exists := table.LeaseFor(req.Object, req.ClientGUID, wanted.Key); exists {
		t.Fatal("stale commit published fresh R while a mutation was active")
	}
	// Failed grant validation leaves the reservation usable. Selecting again
	// with the real gate declines caching rather than leaking the reservation.
	selected, status = table.PrepareLease(token, wanted)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != 0 {
		t.Fatalf("retry acquired unsafe rights: %+v", selected)
	}
	declined, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
	statusIs(t, status, smb.StatusSuccess)
	if declined.LeaseKey != (state.GUID{}) {
		t.Fatalf("declined retry published a lease: %+v", declined)
	}
}

func TestPrepareLeaseWithholdsNewCachingDuringMutation(t *testing.T) {
	for _, requestedState := range []uint32{smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle, 7} {
		t.Run(fmt.Sprintf("requested-%x", requestedState), func(t *testing.T) {
			table := newTable(t)
			writer := request(1)
			writer.ClientGUID, writer.GrantedAccess = state.GUID{9}, 2
			mutator := commit(t, table, writer, state.Grant{})
			mutation, notifications, actions, status := table.BeginMutation(mutator.ID, mutator.Binding)
			statusIs(t, status, smb.StatusSuccess)
			defer table.EndMutation(mutation)
			if len(notifications) != 0 || len(actions) != 0 {
				t.Fatal("an unleased object generated break work")
			}
			req := request(1)
			req.GrantedAccess = 1
			wanted := leaseGrant(req, requestedState).Lease
			wanted.Epoch = 7
			token := reserve(t, table, req)
			selected, status := table.PrepareLease(token, wanted)
			statusIs(t, status, smb.StatusSuccess)
			if selected.State != 0 {
				t.Fatalf("active mutation selected fresh caching: %+v", selected)
			}
			declined, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
			statusIs(t, status, smb.StatusSuccess)
			if declined.LeaseKey != (state.GUID{}) {
				t.Fatalf("declined grant associated a new lease: %+v", declined)
			}
			if _, exists := table.LeaseFor(req.Object, req.ClientGUID, wanted.Key); exists {
				t.Fatal("withheld caching published a lease/epoch")
			}
			table.EndMutation(mutation)
			token = reserve(t, table, req)
			selected, status = table.PrepareLease(token, wanted)
			statusIs(t, status, smb.StatusSuccess)
			want := requestedState
			if want == 7 {
				want = smb.LeaseRead | smb.LeaseHandle
			}
			if selected.State != want {
				t.Fatalf("ended mutation still withheld caching: %+v, want %#x", selected, want)
			}
			open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected})
			statusIs(t, status, smb.StatusSuccess)
			live, exists := table.LeaseFor(req.Object, req.ClientGUID, open.LeaseKey)
			if !exists || live.State != want || live.Epoch != 8 {
				t.Fatalf("released acquisition did not advance once: %+v, exists %v", live, exists)
			}
		})
	}
}
