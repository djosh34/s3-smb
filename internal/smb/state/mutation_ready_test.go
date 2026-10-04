package state_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestMutationReadyRequiresReadRevocationNotJustWriteFlush(t *testing.T) {
	for _, rights := range []uint32{1, 3} {
		table, holder, mutator := mutationOpens(t, rights)
		mutation, notifications, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
		statusIs(t, status, smb.StatusSuccess)
		if len(notifications) != 1 || notifications[0].NewState != 0 || table.MutationNeedsWait(mutation) {
			t.Fatalf("R/RH mutation selection = %+v", notifications)
		}
		// R-only held State is already NONE; RH still awaits its H ACK.
		// Neither fact proves that its READ-revoking packet was delivered.
		if table.MutationReady(mutation) {
			t.Fatalf("state %#x mutation became ready before READ emission", rights)
		}
		table.NoteLeaseBreakDelivered(notifications[0])
		if !table.MutationReady(mutation) {
			t.Fatal("delivered READ revocation still waited for its H ACK")
		}
		if rights == 3 {
			lease, exists := table.LeaseFor(holder.Object, holder.ClientGUID, holder.LeaseKey)
			if !exists || !lease.Breaking || lease.State != 3 {
				t.Fatalf("READ delivery altered held RH or consumed its ACK: %+v", lease)
			}
		}
		_, status = table.Close(holder.ID, holder.Binding)
		statusIs(t, status, smb.StatusSuccess)
		if !table.MutationReady(mutation) {
			t.Fatal("ended lease lifetime kept a READ obligation")
		}
		table.EndMutation(mutation)
		if table.MutationReady(mutation) {
			t.Fatal("ended token remained operation-ready")
		}
	}
}

func TestMutationReadyExcludesOnlyItsOwnLease(t *testing.T) {
	table := newTable(t)
	req := request(1)
	open := commit(t, table, req, leaseGrant(req, 7))
	mutation, notifications, actions, status := table.BeginMutation(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	if len(notifications) != 0 || len(actions) != 0 || !table.MutationReady(mutation) {
		t.Fatal("own ClientGuid/key caching prevented its mutation")
	}
	if table.MutationReady(0) {
		t.Fatal("missing token was treated as ready")
	}
}

func TestQueuedMutationReadyRequiresFinalReadEmission(t *testing.T) {
	table, holder, mutator := mutationOpens(t, 7)
	first := startBreak(t, table, holder.Object, smb.LeaseRead|smb.LeaseHandle)
	mutation, notifications, _, status := table.BeginMutation(mutator.ID, mutator.Binding)
	statusIs(t, status, smb.StatusSuccess)
	defer table.EndMutation(mutation)
	if len(notifications) != 0 || table.MutationReady(mutation) {
		t.Fatal("queued NONE target was treated as delivered revocation")
	}
	continuations, _, status := table.AckBreak(holder.Binding, holder.ClientGUID, holder.LeaseKey, first.NewState)
	statusIs(t, status, smb.StatusSuccess)
	if len(continuations) != 1 || continuations[0].CurrentState != 3 || continuations[0].NewState != 1 || table.MutationNeedsWait(mutation) {
		t.Fatalf("flushed-W RH->R continuation = %+v", continuations)
	}
	if table.MutationReady(mutation) {
		t.Fatal("RH->R preserved READ but authorized actual mutation")
	}
	continuations, _, status = table.AckBreak(holder.Binding, holder.ClientGUID, holder.LeaseKey, 1)
	statusIs(t, status, smb.StatusSuccess)
	if len(continuations) != 1 || continuations[0].CurrentState != 1 || continuations[0].NewState != 0 || continuations[0].AckRequired {
		t.Fatalf("final READ revocation = %+v", continuations)
	}
	if table.MutationReady(mutation) {
		t.Fatal("no-ACK State NONE/wake was treated as final packet delivery")
	}
	table.NoteLeaseBreakDelivered(continuations[0])
	if !table.MutationReady(mutation) {
		t.Fatal("final READ emission failed to release operation readiness")
	}
}
