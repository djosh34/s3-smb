package state_test

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

const (
	leaseR   = smb.LeaseRead
	leaseRH  = smb.LeaseRead | smb.LeaseHandle
	leaseRWH = smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle
)

// Breaks in these tests come from an opener with client and key {9}.
var opener = state.GUID{9}

// leaseGrant asks for a lease with key {3}.
func leaseGrant(req state.OpenRequest, leaseState uint32) state.Grant {
	return state.Grant{Lease: state.Lease{ClientGUID: req.ClientGUID, Key: state.GUID{3}, State: leaseState, Epoch: 7}}
}

func leaseOf(t *testing.T, table *state.Table, open state.Open) state.Lease {
	t.Helper()
	_, lease, status := table.LeaseForOpen(open.ID, open.Binding)
	statusIs(t, status, smb.StatusSuccess)
	return lease
}

// Commit refuses a key that another file took after the CREATE checked it.
func TestCommitRefusesLeaseKeyOfAnotherFile(t *testing.T) {
	table := newTable(t)
	req := request(1)
	reservation := reserve(t, table, request(2))
	commit(t, table, req, leaseGrant(req, leaseR))
	grant := leaseGrant(req, leaseR)
	grant.Handle = &handle{key: smb.ObjectKey{Inode: 2}}
	_, status := table.Commit(reservation, grant)
	statusIs(t, status, smb.StatusInvalidParameter)
}

func TestLeaseBreakWaitsForAcknowledgment(t *testing.T) {
	table := newTable(t)
	req := request(1)
	commit(t, table, req, leaseGrant(req, leaseRWH))
	notification, notify, actions := table.BreakLease(req.Object, opener, opener, leaseRH)
	want := state.Break{Binding: binding, ClientGUID: req.ClientGUID, LeaseKey: state.GUID{3}, CurrentState: leaseRWH, NewState: leaseRH, Epoch: 9, AckRequired: true}
	if !notify || notification != want || len(actions) != 0 {
		t.Fatalf("break = %+v, %v, %+v; want %+v", notification, notify, actions, want)
	}
	if !table.LeaseBreaking(req.Object, opener, opener) || !table.LeaseNeedsBreak(req.Object, opener, opener, leaseRH) {
		t.Fatal("pending break not reported")
	}
	if table.LeaseNeedsBreak(req.Object, req.ClientGUID, state.GUID{3}, 0) {
		t.Fatal("the holder's own open would break its lease")
	}
	if again, restarted, _ := table.BreakLease(req.Object, opener, opener, 0); restarted {
		t.Fatalf("pending break restarted: %+v", again)
	}
	_, status := table.AckBreak(req.ClientGUID, state.GUID{4}, leaseR)
	statusIs(t, status, smb.StatusObjectNameNotFound)
	_, status = table.AckBreak(req.ClientGUID, state.GUID{3}, leaseRWH)
	statusIs(t, status, smb.StatusRequestNotAccepted)
	_, status = table.AckBreak(req.ClientGUID, state.GUID{3}, leaseR)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.AckBreak(req.ClientGUID, state.GUID{3}, leaseR)
	statusIs(t, status, smb.StatusUnsuccessful)
	if table.LeaseBreaking(req.Object, opener, opener) || table.LeaseNeedsBreak(req.Object, opener, opener, leaseRH) {
		t.Fatal("acknowledged break still pending")
	}
	notification, notify, _ = table.BreakLease(req.Object, opener, opener, 0)
	want = state.Break{Binding: binding, ClientGUID: req.ClientGUID, LeaseKey: state.GUID{3}, CurrentState: leaseR, Epoch: 10}
	if !notify || notification != want || table.LeaseBreaking(req.Object, opener, opener) {
		t.Fatalf("losing R = %+v, want %+v without waiting", notification, want)
	}
}

func TestLeaseBreakTimeoutRevokesWholeLease(t *testing.T) {
	table, now := clockTable(t)
	firstReq, secondReq := durableRequest(1, 2), durableRequest(1, 4)
	secondReq.Binding.SessionID = 2
	grant := durableGrant(firstReq)
	grant.Lease.State = leaseRWH
	attached := commit(t, table, firstReq, grant)
	detached := commit(t, table, secondReq, grant)
	table.Disconnect(2)
	notification, notify, actions := table.BreakLease(attached.Object, opener, opener, leaseRH)
	if !notify || notification.Binding != binding || len(actions) != 0 {
		t.Fatalf("break = %+v, %v, %+v", notification, notify, actions)
	}
	*now = now.Add(state.LeaseBreakTimeout - time.Nanosecond)
	if actions := table.ExpireBreaks(); len(actions) != 0 {
		t.Fatalf("break expired early: %+v", actions)
	}
	*now = now.Add(time.Nanosecond)
	if actions := table.ExpireBreaks(); len(actions) != 1 || actions[0].Handle != detached.Handle {
		t.Fatalf("timeout cleanup = %+v", actions)
	}
	if found, status := table.Find(attached.ID, binding); status != smb.StatusSuccess || found.Durable {
		t.Fatalf("attached open after timeout = %+v, %#x", found, status)
	}
	if lease := leaseOf(t, table, attached); lease.State != 0 || lease.Breaking {
		t.Fatalf("lease after timeout = %+v", lease)
	}
}

func TestLosingHandleEndsDurability(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	if notification, notify, _ := table.BreakLease(req.Object, opener, opener, leaseR); !notify {
		t.Fatalf("break = %+v", notification)
	}
	if found, _ := table.Find(open.ID, binding); found.Durable {
		t.Fatal("open stayed durable while losing H")
	}
	if actions := table.Disconnect(binding.SessionID); len(actions) != 1 || table.LeaseBreaking(req.Object, opener, opener) {
		t.Fatalf("drop during the break kept the open: %+v", actions)
	}
}

func TestDetachedLeaseDropsWithoutNotification(t *testing.T) {
	for _, test := range []struct {
		name   string
		target uint32
		closed bool
	}{
		{name: "keep H", target: leaseRH},
		{name: "lose H", target: leaseR, closed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			req := durableRequest(1, 2)
			grant := durableGrant(req)
			grant.Lease.State = leaseRWH
			open := commit(t, table, req, grant)
			table.Disconnect(binding.SessionID)
			_, notify, actions := table.BreakLease(req.Object, opener, opener, test.target)
			if notify || (len(actions) == 1) != test.closed {
				t.Fatalf("detached break notified %v, %+v", notify, actions)
			}
			reattached, status := table.Reconnect(reconnectRequest(open))
			if test.closed {
				statusIs(t, status, smb.StatusObjectNameNotFound)
				return
			}
			statusIs(t, status, smb.StatusSuccess)
			if lease := leaseOf(t, table, reattached); lease.State != leaseRH || lease.Breaking {
				t.Fatalf("lease after reconnect = %+v", lease)
			}
		})
	}
}

func TestSharingLeaseFindsHandleHolder(t *testing.T) {
	table := newTable(t)
	req := request(1)
	req.SharingIntent, req.Sharing = state.RightRead, state.ShareMode(state.RightRead)
	open := commit(t, table, req, leaseGrant(req, leaseRH))
	writer := request(1)
	writer.ClientGUID, writer.SharingIntent = state.GUID{2}, state.RightWrite
	_, status := table.Reserve(writer)
	statusIs(t, status, smb.StatusSharingViolation)
	object, found := table.SharingLease(writer)
	if !found || object != open.Object {
		t.Fatalf("sharing lease = %+v, %v", object, found)
	}
	table.BreakLease(object, state.GUID{}, state.GUID{}, leaseR)
	_, status = table.AckBreak(req.ClientGUID, state.GUID{3}, leaseR)
	statusIs(t, status, smb.StatusSuccess)
	if _, found = table.SharingLease(writer); found {
		t.Fatal("sharing lease reported after H ended")
	}
}
