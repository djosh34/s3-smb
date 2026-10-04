package state_test

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func publicBreak(notification state.Break) state.Break {
	return state.Break{Binding: notification.Binding, ClientGUID: notification.ClientGUID, LeaseKey: notification.LeaseKey, CurrentState: notification.CurrentState, NewState: notification.NewState, Epoch: notification.Epoch, AckRequired: notification.AckRequired}
}

func TestLeaseBreakChangesInitializedAtCreation(t *testing.T) {
	table := newTable(t)
	if table.BreakChanges() == nil {
		t.Fatal("new table has no lease-break change channel")
	}
}

func TestQueuedBreakTimeoutUsesCurrentStageDeadline(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "continuation"}[continuation], func(t *testing.T) {
			checkQueuedBreakTimeout(t, continuation)
		})
	}
}

func checkQueuedBreakTimeout(t *testing.T, continuation bool) {
	t.Helper()
	table, now := clockTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	commit(t, table, req, grant)
	first := startBreak(t, table, req.Object, 3)
	initialDeadline := now.Add(state.LeaseBreakTimeout)
	*now = now.Add(10 * time.Second)
	table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, 0)
	deadline := initialDeadline
	ackState := uint32(3)
	if continuation {
		breaks, _, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, ackState)
		statusIs(t, status, smb.StatusSuccess)
		if len(breaks) != 1 || breaks[0].Epoch != first.Epoch {
			t.Fatalf("continuation: %+v", breaks)
		}
		deadline, ackState = now.Add(state.LeaseBreakTimeout), 1
		*now = initialDeadline
		table.ExpireBreaks()
		if !table.LeasesBreaking(req.Object, state.GUID{9}, state.GUID{9}) {
			t.Fatal("continuation expired at the previous stage's deadline")
		}
	}
	*now = deadline.Add(-time.Nanosecond)
	table.ExpireBreaks()
	if !table.LeasesBreaking(req.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("current stage expired early")
	}
	*now = deadline
	if actions := table.ExpireBreaks(); len(actions) != 0 {
		t.Fatalf("attached nondurable lease timeout cleanup: %+v", actions)
	}
	current, exists := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
	if !exists || current.State != 0 || current.BreakTo != 0 || current.Breaking || !current.Deadline.IsZero() || current.EffectiveState() != 0 {
		t.Fatalf("timeout did not revoke the whole queued lease: %+v", current)
	}
	breaks, actions, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, ackState)
	statusIs(t, status, smb.StatusUnsuccessful)
	if len(breaks) != 0 || len(actions) != 0 {
		t.Fatal("late acknowledgment revived queued work")
	}
}

func TestQueuedBreakDetachedCompletionDiscardsContinuation(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	first := startBreak(t, table, req.Object, 3)
	table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, 0)
	if actions := table.Disconnect(binding.SessionID); len(actions) != 0 {
		t.Fatalf("captured H lease closed before detached completion: %+v", actions)
	}
	actions := table.CompleteDetachedBreak(first)
	if len(actions) != 1 || actions[0].Handle != open.Handle || table.BreakPending(first) {
		t.Fatalf("queued detached completion: %+v", actions)
	}
	breaks, actions, status := table.AckBreak(state.Binding{SessionID: 2}, req.ClientGUID, grant.Lease.Key, 3)
	statusIs(t, status, smb.StatusObjectNameNotFound)
	if len(breaks) != 0 || len(actions) != 0 || len(table.CompleteDetachedBreak(first)) != 0 {
		t.Fatal("completed detached queue returned more work")
	}
}

func TestLeaseSnapshotEffectiveStateIncludesQueuedRevocation(t *testing.T) {
	table, _ := clockTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	commit(t, table, req, grant)
	initial, exists := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
	if !exists || initial.EffectiveState() != 7 {
		t.Fatalf("unbroken effective state: %+v", initial)
	}
	first := startBreak(t, table, req.Object, 3)
	captured, exists := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
	if !exists || captured.EffectiveState() != 3 {
		t.Fatalf("initial captured effective state: %+v", captured)
	}
	table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, 0)
	queued, exists := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
	if !exists || queued.EffectiveState() != 0 || queued.State != 7 || queued.BreakTo != 3 || queued.Epoch != first.Epoch || queued.Deadline != captured.Deadline {
		t.Fatalf("queued NONE incorrectly promises H: %+v, effective %d", queued, queued.EffectiveState())
	}
	if captured.EffectiveState() != 3 || initial.EffectiveState() != 7 {
		t.Fatal("copied snapshots changed with the live lease")
	}
	// Literal pending fixtures with no private queue cannot promise H.
	literal := state.Lease{State: 3, BreakTo: 3, Breaking: true}
	if literal.EffectiveState() != 0 {
		t.Fatal("an uninitialized pending fixture promised H")
	}
}

func TestQueuedBreakUsesCapturedAcknowledgmentAndStagedTargets(t *testing.T) {
	table, now := clockTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	grant.Lease.Epoch = 7
	commit(t, table, req, grant)
	first := startBreak(t, table, req.Object, 3)
	*now = now.Add(10 * time.Second)
	if breaks, actions := table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, 0); len(breaks) != 0 || len(actions) != 0 {
		t.Fatal("queued overwrite sent a replacement break")
	}
	breaks, actions, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, 3)
	statusIs(t, status, smb.StatusSuccess)
	want := state.Break{Binding: binding, ClientGUID: req.ClientGUID, LeaseKey: grant.Lease.Key, CurrentState: 3, NewState: 1, Epoch: first.Epoch, AckRequired: true}
	if len(actions) != 0 || len(breaks) != 1 || publicBreak(breaks[0]) != want {
		t.Fatalf("RH acknowledgment continuation: %+v, cleanup %+v, want %+v", breaks, actions, want)
	}
	current, exists := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
	if !exists || current.State != 3 || current.BreakTo != 1 || current.Epoch != first.Epoch || !current.Breaking || current.Deadline != now.Add(state.LeaseBreakTimeout) {
		t.Fatalf("new ACK stage: %+v", current)
	}
	*now = now.Add(2 * time.Second)
	breaks, actions, status = table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, 1)
	statusIs(t, status, smb.StatusSuccess)
	want.CurrentState, want.NewState, want.AckRequired = 1, 0, false
	if len(actions) != 0 || len(breaks) != 1 || publicBreak(breaks[0]) != want {
		t.Fatalf("R acknowledgment continuation: %+v, cleanup %+v, want %+v", breaks, actions, want)
	}
	current, exists = table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
	if !exists || current.State != 0 || current.BreakTo != 0 || current.Epoch != first.Epoch || current.Breaking || !current.Deadline.IsZero() || table.LeasesBreaking(req.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatalf("R-only final break still requires acknowledgment: %+v", current)
	}
}

func TestQueuedBreakSignalsOnlyWhenUltimateTargetChanges(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, 7)
	commit(t, table, req, grant)
	startBreak(t, table, req.Object, 3)
	changed := table.BreakChanges()
	table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, 0)
	select {
	case <-changed:
	default:
		t.Fatal("queuing stronger revocation did not wake a break observer")
	}
	changed = table.BreakChanges()
	for _, target := range []uint32{0, 3} {
		table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, target)
		select {
		case <-changed:
			t.Fatal("an unchanged queue woke a break observer")
		default:
		}
	}
}

func TestPendingBreakKeepsCapturedTargetEpochAndDeadline(t *testing.T) {
	table, now := clockTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite)
	grant.Lease.Epoch = 7
	commit(t, table, req, grant)
	first := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	captured, exists := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
	if !exists || captured.State != 7 || captured.BreakTo != 3 || !captured.Breaking || captured.Deadline != now.Add(state.LeaseBreakTimeout) {
		t.Fatalf("first pending break: %+v", captured)
	}
	*now = now.Add(10 * time.Second)
	for _, target := range []uint32{0, smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle} {
		breaks, actions := table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, target)
		if len(breaks) != 0 || len(actions) != 0 {
			t.Fatalf("queued target %d replaced the notification: %+v, %+v", target, breaks, actions)
		}
		current, found := table.LeaseFor(req.Object, req.ClientGUID, grant.Lease.Key)
		if !found || current.State != captured.State || current.BreakTo != captured.BreakTo || current.Epoch != first.Epoch || current.Deadline != captured.Deadline || !current.Breaking {
			t.Fatalf("queued target %d changed the captured break: %+v", target, current)
		}
	}
}
