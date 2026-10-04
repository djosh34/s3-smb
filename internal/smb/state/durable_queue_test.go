package state_test

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestDurableCommitUsesQueuedEffectiveH(t *testing.T) {
	table, now := clockTable(t)
	req := request(1)
	initial := leaseGrant(req, 7)
	initial.Lease.Epoch = 8
	original := commit(t, table, req, initial)
	first := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	_, captured, status := table.LeaseForOpen(original.ID, original.Binding)
	statusIs(t, status, smb.StatusSuccess)
	req.CreateGUID = state.GUID{5}
	token := reserve(t, table, req)
	selected, status := table.PrepareLease(token, initial.Lease)
	statusIs(t, status, smb.StatusSuccess)
	if selected.State != 0 || !table.DurableEligible(token, selected) {
		t.Fatalf("captured RH did not retain H before queuing: %+v", selected)
	}
	*now = now.Add(10 * time.Second)
	breaks, actions := table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, 0)
	if len(breaks) != 0 || len(actions) != 0 {
		t.Fatalf("queued NONE replaced notification: %+v, %+v", breaks, actions)
	}
	_, queued, status := table.LeaseForOpen(original.ID, original.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if queued.State != 7 || queued.BreakTo != smb.LeaseRead|smb.LeaseHandle || queued.Epoch != first.Epoch || queued.Deadline != captured.Deadline || queued.EffectiveState() != 0 {
		t.Fatalf("queued NONE did not retain capture/revoke effective H: %+v", queued)
	}
	if table.DurableEligible(token, selected) {
		t.Error("queued NONE still authorized new durability from captured RH")
	}
	_, status = table.Commit(token, state.Grant{Handle: &handle{key: req.Object}, Lease: selected, DurableTimeout: smb.DefaultDurableTimeout})
	statusIs(t, status, smb.StatusInvalidParameter)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
}
