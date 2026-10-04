package state_test

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func mutationOpens(t *testing.T, rights uint32) (*state.Table, state.Open, state.Open) {
	t.Helper()
	table := newTable(t)
	req := request(1)
	holder := commit(t, table, req, leaseGrant(req, rights))
	metadata := req
	metadata.ClientGUID, metadata.GrantedAccess = state.GUID{8}, 0x80
	mutator := commit(t, table, metadata, state.Grant{})
	return table, holder, mutator
}

func TestBeginMutationCapturesBreakAndHeldWriteWait(t *testing.T) {
	for _, rights := range []uint32{1, 3, 7} {
		t.Run(fmt.Sprintf("state_%d", rights), func(t *testing.T) {
			table, holder, mutator := mutationOpens(t, rights)
			mutation, notifications, actions, status := table.BeginMutation(mutator.ID, mutator.Binding)
			statusIs(t, status, smb.StatusSuccess)
			defer table.EndMutation(mutation)
			if mutation == 0 || len(actions) != 0 || len(notifications) != 1 {
				t.Fatalf("mutation selection = %d, %+v, %+v", mutation, notifications, actions)
			}
			notification := notifications[0]
			if notification.LeaseKey != holder.LeaseKey || notification.CurrentState != rights || notification.NewState != 0 || notification.AckRequired != (rights != 1) {
				t.Fatalf("captured invalidation = %+v", notification)
			}
			if table.MutationNeedsWait(mutation) != (rights == 7) {
				t.Fatalf("held W wait for state %#x", rights)
			}
			if rights != 1 {
				_, _, status = table.AckBreak(holder.Binding, holder.ClientGUID, holder.LeaseKey, 0)
				statusIs(t, status, smb.StatusSuccess)
			}
			if table.MutationNeedsWait(mutation) {
				t.Fatal("acknowledged mutation retained a W barrier")
			}
		})
	}
}

func TestMutationWaitUsesHeldWriteNotEffectiveState(t *testing.T) {
	table, holder, mutator := mutationOpens(t, 7)
	first := startBreak(t, table, holder.Object, smb.LeaseRead|smb.LeaseHandle)
	mutation, notifications, actions, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	if len(notifications) != 0 || len(actions) != 0 {
		t.Fatal("stronger mutation replaced the captured pending break")
	}
	lease, exists := table.LeaseFor(holder.Object, holder.ClientGUID, holder.LeaseKey)
	if !exists || lease.EffectiveState() != 0 || !table.MutationNeedsWait(mutation) {
		t.Fatalf("pending held W barrier was lost: %+v", lease)
	}
	continuations, _, status := table.AckBreak(holder.Binding, holder.ClientGUID, holder.LeaseKey, first.NewState)
	statusIs(t, status, smb.StatusSuccess)
	if len(continuations) != 1 || continuations[0].CurrentState != 3 || continuations[0].NewState != 1 || !continuations[0].AckRequired {
		t.Fatalf("RH continuation = %+v", continuations)
	}
	if table.MutationNeedsWait(mutation) {
		t.Fatal("RH-only continuation incorrectly waits for its ACK")
	}
}

func TestMutationTokensSurviveCloseAndReleaseIndependently(t *testing.T) {
	table, holder, mutator := mutationOpens(t, 7)
	first, _, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	second, _, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if first == second {
		t.Fatal("concurrent mutations reused a token")
	}
	_, status = table.Close(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if !table.MutationNeedsWait(first) || !table.MutationNeedsWait(second) {
		t.Fatal("close prematurely released an active mutation token")
	}
	table.EndMutation(first)
	table.EndMutation(first)
	if table.MutationNeedsWait(first) || !table.MutationNeedsWait(second) {
		t.Fatal("ending one token released another mutation")
	}
	_, _, status = table.AckBreak(holder.Binding, holder.ClientGUID, holder.LeaseKey, 0)
	statusIs(t, status, smb.StatusSuccess)
	table.EndMutation(second)
	table.EndMutation(0)
}

func TestBeginMutationValidatesBindingAndExcludesSameLease(t *testing.T) {
	table, holder, _ := mutationOpens(t, 7)
	wrong := holder.ID
	wrong.Volatile++
	for _, test := range []struct {
		id      state.FileID
		binding state.Binding
	}{
		{id: wrong, binding: holder.Binding},
		{id: holder.ID, binding: state.Binding{SessionID: 99, TreeID: 1}},
		{id: holder.ID},
	} {
		mutation, notifications, actions, status := table.BeginMutation(test.id, test.binding)
		statusIs(t, status, smb.StatusFileClosed)
		if mutation != 0 || len(notifications) != 0 || len(actions) != 0 {
			t.Fatal("invalid binding acquired mutation work")
		}
	}
	mutation, notifications, actions, status := table.BeginMutation(holder.ID, holder.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	if len(notifications) != 0 || len(actions) != 0 || table.MutationNeedsWait(mutation) {
		t.Fatal("own ClientGuid/key mutation broke its shared lease")
	}
	lease, exists := table.LeaseFor(holder.Object, holder.ClientGUID, holder.LeaseKey)
	if !exists || lease.State != 7 || lease.Breaking {
		t.Fatalf("own lease changed: %+v", lease)
	}
}
