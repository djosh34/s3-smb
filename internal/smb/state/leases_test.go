package state_test

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func leaseGrant(req state.OpenRequest, leaseState uint32) state.Grant {
	return state.Grant{Lease: state.Lease{ClientGUID: req.ClientGUID, Key: state.GUID{3}, State: leaseState}}
}

func startBreak(t *testing.T, table *state.Table, object smb.ObjectKey, target uint32) state.Break {
	t.Helper()
	breaks, actions := table.BreakLeases(object, state.GUID{9}, state.GUID{9}, target)
	if len(actions) != 0 {
		t.Fatalf("unexpected immediate break cleanup: %+v", actions)
	}
	if len(breaks) != 1 {
		t.Fatalf("break count = %d, want 1", len(breaks))
	}
	return breaks[0]
}

func TestLeaseBreakCapturesNotificationAndAcknowledgesWithoutEpoch(t *testing.T) {
	for _, test := range []struct {
		name    string
		current uint32
		target  uint32
		ack     bool
	}{
		{name: "R to none", current: smb.LeaseRead},
		{name: "RWH to RH", current: smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle, target: smb.LeaseRead | smb.LeaseHandle, ack: true},
		{name: "RWH to R", current: smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle, target: smb.LeaseRead, ack: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			req := request(1)
			grant := leaseGrant(req, test.current)
			grant.Lease.Epoch = 7
			commit(t, table, req, grant)
			notification := startBreak(t, table, req.Object, test.target)
			if notification.CurrentState != test.current || notification.NewState != test.target || notification.AckRequired != test.ack || notification.Epoch != 8 || notification.Binding != binding || notification.ClientGUID != req.ClientGUID || notification.LeaseKey != grant.Lease.Key {
				t.Fatalf("notification: %+v", notification)
			}
			if breaks, actions := table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, test.target); len(breaks) != 0 || len(actions) != 0 {
				t.Fatal("duplicate break notification")
			}
			if test.ack {
				_, actions, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, test.target)
				statusIs(t, status, smb.StatusSuccess)
				if len(actions) != 0 {
					t.Fatal("non-durable acknowledgement returned cleanup")
				}
				_, _, status = table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, test.target)
				statusIs(t, status, smb.StatusUnsuccessful)
			} else {
				_, _, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, 0)
				statusIs(t, status, smb.StatusUnsuccessful)
			}
			if notification.CurrentState != test.current {
				t.Fatal("returned notification was mutated")
			}
			// A metadata open is safe after W has ended.
			commit(t, table, request(1), state.Grant{})
		})
	}
}

func TestAckBreakChecksIdentityAndSubset(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite)
	commit(t, table, req, grant)
	startBreak(t, table, req.Object, smb.LeaseRead)
	for _, test := range []struct {
		binding state.Binding
		client  state.GUID
		key     state.GUID
		state   uint32
		want    smb.Status
	}{
		{binding: state.Binding{SessionID: 2, TreeID: 1}, client: req.ClientGUID, key: grant.Lease.Key, state: smb.LeaseRead, want: smb.StatusInvalidParameter},
		{binding: state.Binding{SessionID: 1, TreeID: 2}, client: req.ClientGUID, key: grant.Lease.Key, state: smb.LeaseRead, want: smb.StatusInvalidParameter},
		{binding: binding, client: state.GUID{9}, key: grant.Lease.Key, state: smb.LeaseRead, want: smb.StatusObjectNameNotFound},
		{binding: binding, client: req.ClientGUID, key: state.GUID{9}, state: smb.LeaseRead, want: smb.StatusObjectNameNotFound},
		{binding: binding, client: req.ClientGUID, key: grant.Lease.Key, state: smb.LeaseRead | smb.LeaseHandle, want: 0xc00000d0},
		{binding: state.Binding{}, client: req.ClientGUID, key: grant.Lease.Key, state: smb.LeaseRead, want: smb.StatusInvalidParameter},
	} {
		_, _, status := table.AckBreak(test.binding, test.client, test.key, test.state)
		statusIs(t, status, test.want)
	}
	_, _, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, 0)
	statusIs(t, status, smb.StatusSuccess)
}

func TestRejectedAckDoesNotExtendBreakDeadline(t *testing.T) {
	table, now := clockTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	*now = now.Add(10 * time.Second)
	_, actions, status := table.AckBreak(binding, req.ClientGUID, open.LeaseKey, grant.Lease.State)
	statusIs(t, status, 0xc00000d0)
	if len(actions) != 0 {
		t.Fatalf("rejected acknowledgment returned cleanup: %+v", actions)
	}
	*now = now.Add(state.LeaseBreakTimeout - 10*time.Second)
	table.ExpireBreaks()
	found, status := table.Find(open.ID, binding)
	statusIs(t, status, smb.StatusSuccess)
	if found.Durable {
		t.Fatal("rejected acknowledgment extended or cleared the pending break")
	}
	writer := request(1)
	writer.GrantedAccess = 2
	writer.ClientGUID = state.GUID{9}
	commit(t, table, writer, state.Grant{})
}

func TestBreakTimeoutRevokesWholeLease(t *testing.T) {
	table, now := clockTable(t)
	attachedReq := durableRequest(1, 2)
	attachedGrant := durableGrant(attachedReq)
	attachedGrant.Lease.State |= smb.LeaseWrite
	attached := commit(t, table, attachedReq, attachedGrant)
	detachedReq := durableRequest(2, 4)
	detachedReq.Binding.SessionID = 2
	detachedReq.GrantedAccess |= 0x10000
	detachedReq.SharingIntent |= state.RightDelete
	detachedGrant := durableGrant(detachedReq)
	detachedGrant.Lease.State |= smb.LeaseWrite
	detachedGrant.DeleteOnClose, detachedGrant.DeleteName = true, deleteName("")
	detached := commit(t, table, detachedReq, detachedGrant)
	table.Disconnect(2)
	startBreak(t, table, attached.Object, smb.LeaseRead|smb.LeaseHandle)
	notification := startBreak(t, table, detached.Object, smb.LeaseRead|smb.LeaseHandle)
	if notification.Binding != (state.Binding{}) || !notification.AckRequired {
		t.Fatalf("detached notification: %+v", notification)
	}
	*now = now.Add(state.LeaseBreakTimeout - time.Nanosecond)
	if len(table.ExpireBreaks()) != 0 {
		t.Fatal("break expired early")
	}
	*now = now.Add(time.Nanosecond)
	actions := table.ExpireBreaks()
	if len(actions) != 1 || actions[0].Handle != detached.Handle || !actions[0].Remove || actions[0].Name != detachedGrant.DeleteName {
		t.Fatalf("break timeout cleanup: %+v", actions)
	}
	found, status := table.Find(attached.ID, binding)
	statusIs(t, status, smb.StatusSuccess)
	if found.Durable || found.DurableTimeout != 0 {
		t.Fatal("attached open kept durability after losing H")
	}
	if len(table.ExpireBreaks()) != 0 {
		t.Fatal("break cleanup repeated")
	}
	writer := request(1)
	writer.GrantedAccess = 2
	writer.Binding.SessionID = 3
	commit(t, table, writer, state.Grant{})
	if actions = table.Disconnect(1); len(actions) != 1 || actions[0].Handle != attached.Handle {
		t.Fatalf("non-durable drop cleanup: %+v", actions)
	}
}

func TestSharedLeaseTimeoutRevokesAttachedAndDetachedMembers(t *testing.T) {
	table, now := clockTable(t)
	firstReq := durableRequest(1, 2)
	grant := durableGrant(firstReq)
	grant.Lease.State |= smb.LeaseWrite
	first := commit(t, table, firstReq, grant)
	secondReq := firstReq
	secondReq.CreateGUID = state.GUID{4}
	secondReq.Binding.SessionID = 2
	second := commit(t, table, secondReq, grant)
	if actions := table.Disconnect(2); len(actions) != 0 {
		t.Fatalf("shared durable member closed before timeout: %+v", actions)
	}
	startBreak(t, table, first.Object, smb.LeaseRead|smb.LeaseHandle)
	*now = now.Add(state.LeaseBreakTimeout)
	actions := table.ExpireBreaks()
	if len(actions) != 1 || actions[0].Handle != second.Handle {
		t.Fatalf("shared lease timeout cleanup: %+v", actions)
	}
	found, status := table.Find(first.ID, binding)
	statusIs(t, status, smb.StatusSuccess)
	if found.Durable {
		t.Fatal("attached member retained H after timeout")
	}
	_, status = table.Reconnect(reconnectRequest(second))
	statusIs(t, status, smb.StatusObjectNameNotFound)
	writer := request(1)
	writer.GrantedAccess = 2
	writer.ClientGUID = state.GUID{9}
	commit(t, table, writer, state.Grant{})
}

func TestFullyDetachedHandleBreakClosesImmediately(t *testing.T) {
	table, _ := clockTable(t)
	req := durableRequest(1, 2)
	req.GrantedAccess = 1
	req.Sharing = state.ShareMode(state.RightRead | state.RightDelete)
	open := commit(t, table, req, durableGrant(req))
	observerReq := request(1)
	observerReq.Binding.SessionID = 2
	observer := commit(t, table, observerReq, state.Grant{})
	statusIs(t, table.Lock(open.ID, binding, []state.Range{{Length: 10, Exclusive: true}}, false), smb.StatusSuccess)
	statusIs(t, table.CheckIO(observer.ID, observer.Binding, 1, 1, false), smb.StatusFileLockConflict)
	writerReq := request(1)
	writerReq.GrantedAccess = 2
	writerReq.Binding.SessionID = 3
	_, status := table.Reserve(writerReq)
	statusIs(t, status, smb.StatusSharingViolation)
	if len(table.Disconnect(1)) != 0 {
		t.Fatal("durable open closed before a break")
	}
	breaks, actions := table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, smb.LeaseRead)
	if len(breaks) != 0 || len(actions) != 1 || actions[0].Handle != open.Handle {
		t.Fatalf("fully detached break: notifications %+v, cleanup %+v", breaks, actions)
	}
	_, status = table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusObjectNameNotFound)
	statusIs(t, table.CheckIO(observer.ID, observer.Binding, 1, 1, false), smb.StatusSuccess)
	commit(t, table, writerReq, state.Grant{})
	if len(table.ExpireBreaks()) != 0 || len(table.Expire()) != 0 {
		t.Fatal("immediate cleanup ran twice")
	}
}

func TestFullyDetachedSharedLeaseAppliesPendingDeletion(t *testing.T) {
	table, _ := clockTable(t)
	firstReq := durableRequest(1, 2)
	firstReq.GrantedAccess |= 0x10000
	grant := durableGrant(firstReq)
	grant.DeleteOnClose, grant.DeleteName = true, deleteName("")
	first := commit(t, table, firstReq, grant)
	secondReq := durableRequest(1, 4)
	second := commit(t, table, secondReq, durableGrant(firstReq))
	table.Disconnect(1)
	breaks, actions := table.BreakLeases(first.Object, state.GUID{9}, state.GUID{9}, 0)
	if len(breaks) != 0 || len(actions) != 2 || actions[0].Handle != first.Handle || actions[1].Handle != second.Handle || actions[0].Remove || !actions[1].Remove || actions[1].Name != grant.DeleteName {
		t.Fatalf("detached shared lease cleanup: %+v, %+v", breaks, actions)
	}
	_, status := table.Reconnect(reconnectRequest(first))
	statusIs(t, status, smb.StatusObjectNameNotFound)
	_, status = table.Reconnect(reconnectRequest(second))
	statusIs(t, status, smb.StatusObjectNameNotFound)
}

func TestDisconnectDuringHandleBreakClosesLastDetachedMember(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	startBreak(t, table, req.Object, smb.LeaseRead)
	actions := table.Disconnect(1)
	if len(actions) != 1 || actions[0].Handle != open.Handle {
		t.Fatalf("disconnect during H break: %+v", actions)
	}
	_, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusObjectNameNotFound)
}

func TestAckDroppingHClosesDetachedMembers(t *testing.T) {
	table := newTable(t)
	firstReq := durableRequest(1, 2)
	grant := durableGrant(firstReq)
	first := commit(t, table, firstReq, grant)
	secondReq := durableRequest(1, 4)
	secondReq.Binding.SessionID = 2
	second := commit(t, table, secondReq, grant)
	table.Disconnect(1)
	notification := startBreak(t, table, first.Object, smb.LeaseRead)
	if notification.Binding != second.Binding {
		t.Fatalf("notification chose detached member: %+v", notification)
	}
	_, actions, status := table.AckBreak(second.Binding, second.ClientGUID, second.LeaseKey, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
	if len(actions) != 1 || actions[0].Handle != first.Handle {
		t.Fatalf("ack cleanup: %+v", actions)
	}
	found, status := table.Find(second.ID, second.Binding)
	statusIs(t, status, smb.StatusSuccess)
	if found.Durable {
		t.Fatal("attached lease member remained durable")
	}
	_, status = table.Reconnect(reconnectRequest(first))
	statusIs(t, status, smb.StatusObjectNameNotFound)
}

func TestLeaseKeyIsSharedOnlyOnOneObject(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle)
	first := commit(t, table, req, grant)
	second := commit(t, table, req, grant)
	if first.LeaseKey != second.LeaseKey {
		t.Fatal("same lease was not shared")
	}
	if breaks, actions := table.BreakLeases(req.Object, req.ClientGUID, grant.Lease.Key, 0); len(breaks) != 0 || len(actions) != 0 {
		t.Fatal("requesting lease was broken")
	}
	otherReq := request(2)
	token := reserve(t, table, otherReq)
	grant.Handle = &handle{key: otherReq.Object}
	_, status := table.Commit(token, grant)
	statusIs(t, status, smb.StatusInvalidParameter)
	statusIs(t, table.Abort(token), smb.StatusSuccess)
	closeOpen(t, table, first)
	startBreak(t, table, req.Object, smb.LeaseRead)
	_, _, status = table.AckBreak(binding, req.ClientGUID, second.LeaseKey, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
	closeOpen(t, table, second)
	commit(t, table, otherReq, grant)
}

func TestLeaseGrantsCannotAcquireConflictingRights(t *testing.T) {
	for _, test := range []struct {
		name       string
		rights     state.Rights
		leaseState uint32
	}{
		{name: "read cache with writer", rights: state.RightWrite, leaseState: smb.LeaseRead},
		{name: "write cache with reader", rights: state.RightRead, leaseState: smb.LeaseRead | smb.LeaseHandle | smb.LeaseWrite},
		{name: "write cache with metadata open", leaseState: smb.LeaseRead | smb.LeaseHandle | smb.LeaseWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, committed := range []bool{false, true} {
				table := newTable(t)
				otherReq := request(1)
				otherReq.SharingIntent = test.rights
				if committed {
					commit(t, table, otherReq, state.Grant{})
				} else {
					reserve(t, table, otherReq)
				}
				req := request(1)
				req.ClientGUID = state.GUID{2}
				grant := leaseGrant(req, test.leaseState)
				grant.Handle = &handle{key: req.Object}
				token := reserve(t, table, req)
				_, status := table.Commit(token, grant)
				statusIs(t, status, smb.StatusInvalidParameter)
				statusIs(t, table.Abort(token), smb.StatusSuccess)
			}
		})
	}
}

func TestConflictCannotCommitUntilLeaseBreakEnds(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite)
	commit(t, table, req, grant)
	otherReq := request(1)
	otherReq.ClientGUID = state.GUID{2}
	otherReq.SharingIntent = state.RightWrite
	token := reserve(t, table, otherReq)
	otherGrant := state.Grant{Handle: &handle{key: req.Object}}
	_, status := table.Commit(token, otherGrant)
	statusIs(t, status, smb.StatusSharingViolation)
	startBreak(t, table, req.Object, 0)
	_, status = table.Commit(token, otherGrant)
	statusIs(t, status, smb.StatusSharingViolation)
	_, _, status = table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, 0)
	statusIs(t, status, smb.StatusSuccess)
	_, status = table.Commit(token, otherGrant)
	statusIs(t, status, smb.StatusSuccess)
}

func TestPendingLeaseGrantCannotExceedTarget(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite)
	commit(t, table, req, grant)
	startBreak(t, table, req.Object, smb.LeaseRead)
	token := reserve(t, table, req)
	grant.Handle = &handle{key: req.Object}
	_, status := table.Commit(token, grant)
	statusIs(t, status, smb.StatusInvalidParameter)
	grant.Lease.State = smb.LeaseRead
	_, status = table.Commit(token, grant)
	statusIs(t, status, smb.StatusSuccess)
	_, _, status = table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
}

func TestBreakTargetOnlyLosesRights(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite)
	grant.Lease.Epoch = 0xffff
	commit(t, table, req, grant)
	first := startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	if first.Epoch != 0 {
		t.Fatalf("epoch did not wrap: %d", first.Epoch)
	}
	for _, target := range []uint32{smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle} {
		if breaks, actions := table.BreakLeases(req.Object, state.GUID{9}, state.GUID{9}, target); len(breaks) != 0 || len(actions) != 0 {
			t.Fatal("pending break emitted another notification")
		}
	}
	breaks, actions, status := table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, smb.LeaseRead|smb.LeaseHandle)
	statusIs(t, status, smb.StatusSuccess)
	if len(actions) != 0 || len(breaks) != 1 || breaks[0].CurrentState != 3 || breaks[0].NewState != 1 || breaks[0].Epoch != first.Epoch {
		t.Fatalf("queued break regained H or changed epoch: %+v, %+v", breaks, actions)
	}
	_, _, status = table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, smb.LeaseRead|smb.LeaseHandle)
	statusIs(t, status, smb.StatusRequestNotAccepted)
	breaks, actions, status = table.AckBreak(binding, req.ClientGUID, grant.Lease.Key, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
	if len(breaks) != 0 || len(actions) != 0 {
		t.Fatal("acknowledged queue returned more work")
	}
	last := startBreak(t, table, req.Object, 0)
	if last.CurrentState != smb.LeaseRead || last.AckRequired || last.Epoch != 1 {
		t.Fatalf("independent R-only break after downgrade: %+v", last)
	}
}

func TestLeaseUpgradeAdvancesServerEpoch(t *testing.T) {
	table := newTable(t)
	req := request(1)
	grant := leaseGrant(req, smb.LeaseRead)
	grant.Lease.Epoch = 7
	first := commit(t, table, req, grant)
	grant.Lease.State = smb.LeaseRead | smb.LeaseHandle
	grant.Lease.Epoch = 100
	second := commit(t, table, req, grant)
	closeOpen(t, table, first)
	notification := startBreak(t, table, req.Object, 0)
	if notification.Epoch != 9 || notification.CurrentState != smb.LeaseRead|smb.LeaseHandle {
		t.Fatalf("upgrade notification: %+v", notification)
	}
	_, _, status := table.AckBreak(second.Binding, second.ClientGUID, second.LeaseKey, 0)
	statusIs(t, status, smb.StatusSuccess)
}

func TestHandleBreakCanRetainReadAndWriteCaching(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	notification := startBreak(t, table, open.Object, smb.LeaseRead|smb.LeaseWrite)
	if notification.CurrentState != smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite || notification.NewState != smb.LeaseRead|smb.LeaseWrite || !notification.AckRequired {
		t.Fatalf("handle-only break: %+v", notification)
	}
	_, _, status := table.AckBreak(binding, open.ClientGUID, open.LeaseKey, smb.LeaseRead|smb.LeaseWrite)
	statusIs(t, status, smb.StatusSuccess)
	found, status := table.Find(open.ID, binding)
	statusIs(t, status, smb.StatusSuccess)
	if found.Durable {
		t.Fatal("RW lease kept durability without H")
	}
	notification = startBreak(t, table, open.Object, smb.LeaseRead)
	if notification.CurrentState != smb.LeaseRead|smb.LeaseWrite || !notification.AckRequired {
		t.Fatalf("write break after H ended: %+v", notification)
	}
}

func TestOldBindingCannotAcknowledgeAfterReconnect(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	grant := durableGrant(req)
	grant.Lease.State |= smb.LeaseWrite
	open := commit(t, table, req, grant)
	// Keep H while breaking W so the detached open can still reconnect.
	startBreak(t, table, req.Object, smb.LeaseRead|smb.LeaseHandle)
	table.Disconnect(1)
	fresh, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	_, _, status = table.AckBreak(binding, req.ClientGUID, open.LeaseKey, smb.LeaseRead)
	statusIs(t, status, smb.StatusInvalidParameter)
	_, _, status = table.AckBreak(fresh.Binding, fresh.ClientGUID, fresh.LeaseKey, smb.LeaseRead)
	statusIs(t, status, smb.StatusSuccess)
}

func TestInvalidLeaseGrantsDoNotChangeState(t *testing.T) {
	for _, test := range []struct {
		modify func(*state.OpenRequest, *state.Grant)
		name   string
	}{
		{name: "W alone", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.State = smb.LeaseWrite }},
		{name: "H alone", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.State = smb.LeaseHandle }},
		{name: "unknown rights", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.State = 8 }},
		{name: "wrong client", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.ClientGUID = state.GUID{9} }},
		{name: "zero key", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.Key = state.GUID{} }},
		{name: "breaking grant", modify: func(_ *state.OpenRequest, grant *state.Grant) { grant.Lease.Breaking = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			req := request(1)
			grant := leaseGrant(req, smb.LeaseRead)
			test.modify(&req, &grant)
			grant.Handle = &handle{key: req.Object}
			token := reserve(t, table, req)
			_, status := table.Commit(token, grant)
			statusIs(t, status, smb.StatusInvalidParameter)
			statusIs(t, table.Abort(token), smb.StatusSuccess)
			commit(t, table, request(1), state.Grant{})
		})
	}
}
