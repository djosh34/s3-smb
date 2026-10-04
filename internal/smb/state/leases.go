package state

import (
	"slices"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// LeaseBreakTimeout bounds an unacknowledged break, as in MS-SMB2.
const LeaseBreakTimeout = 35 * time.Second

// EffectiveState reports rights left after pending revocations, without
// changing the captured held state, notification target or epoch.
func (lease Lease) EffectiveState() uint32 {
	if lease.Breaking {
		return lease.State & lease.BreakTo & lease.queuedTo
	}
	return lease.State
}

func validLeaseState(state uint32) bool {
	return state == 0 || state == smb.LeaseRead || state == smb.LeaseRead|smb.LeaseHandle || state == smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite
}

func (table *Table) validateLease(request OpenRequest, reservation Reservation, grant Grant) smb.Status {
	lease := grant.Lease
	if !validLeaseState(lease.State) || lease.Breaking {
		return smb.StatusInvalidParameter
	}
	if !table.leasesAllow(request, lease) {
		return smb.StatusSharingViolation
	}
	if status := table.validateLeaseMetadata(request, grant); status != smb.StatusSuccess {
		return status
	}
	identity := leaseIdentity{client: lease.ClientGUID, key: lease.Key}
	if current := table.lease(request.Object, identity); current != nil {
		if (current.Breaking && lease.State & ^current.BreakTo != 0) || (!current.Breaking && lease.State != 0 && current.State & ^lease.State != 0) {
			return smb.StatusInvalidParameter
		}
		if lease.State == 0 || !current.Breaking && lease.State == current.State {
			// Membership alone does not acquire fresh caching rights. A
			// foreign reservation must not block a same-key held-state join.
			return smb.StatusSuccess
		}
	}
	if lease.State != 0 && !table.leaseMutationAllows(request.Object, lease.ClientGUID, lease.Key) {
		// PrepareLease's snapshot may predate BeginMutation. Commit must
		// not publish new caching while that operation still owns its gate.
		return smb.StatusInvalidParameter
	}
	for _, open := range table.opens {
		if open.Object == request.Object && (open.ClientGUID != lease.ClientGUID || open.LeaseKey != lease.Key) && !leaseCompatible(lease.State, open.SharingIntent) {
			return smb.StatusInvalidParameter
		}
	}
	for token, reserved := range table.reservations {
		if token != reservation && reserved.Object == request.Object && !leaseCompatible(lease.State, reserved.SharingIntent) {
			return smb.StatusInvalidParameter
		}
	}
	return smb.StatusSuccess
}

// validateLeaseMetadata checks the proposal's shape/identity before any safe
// reselection. Compatibility with current cache rights is checked separately.
func (table *Table) validateLeaseMetadata(request OpenRequest, grant Grant) smb.Status {
	lease := grant.Lease
	if !validLeaseState(lease.State) || lease.Breaking {
		return smb.StatusInvalidParameter
	}
	identity := leaseIdentity{client: lease.ClientGUID, key: lease.Key}
	if lease.State == 0 && table.lease(request.Object, identity) == nil {
		return smb.StatusSuccess
	}
	if grant.Directory || request.Object.Stream != "" || lease.Key == (GUID{}) || lease.ClientGUID != request.ClientGUID {
		return smb.StatusInvalidParameter
	}
	if object, exists := table.leaseObjects[identity]; exists && object != request.Object {
		return smb.StatusInvalidParameter
	}
	return smb.StatusSuccess
}

func (table *Table) leasesAllow(request OpenRequest, joining Lease) bool {
	// MS-FSA 2.1.4.12's metadata-only OPEN exemption applies to held
	// granular leases, not acquisition of W beside another open.
	if joining.State == 0 && request.SharingIntent == 0 && request.GrantedAccess & ^uint32(0x00120180) == 0 {
		return true
	}
	record := table.objects[request.Object]
	if record == nil {
		return true
	}
	for _, held := range record.Leases {
		if held.ClientGUID == joining.ClientGUID && held.Key == joining.Key && joining.Key != (GUID{}) {
			continue
		}
		if !leaseCompatible(held.State, request.SharingIntent) {
			return false
		}
	}
	return true
}

func leaseCompatible(state uint32, _ Rights) bool {
	return state&smb.LeaseWrite == 0
}

func (table *Table) lease(object smb.ObjectKey, identity leaseIdentity) *Lease {
	record := table.objects[object]
	if record != nil {
		for index := range record.Leases {
			lease := &record.Leases[index]
			if lease.ClientGUID == identity.client && lease.Key == identity.key {
				return lease
			}
		}
	}
	return nil
}

func (table *Table) commitLease(object smb.ObjectKey, grant Lease) {
	identity := leaseIdentity{client: grant.ClientGUID, key: grant.Key}
	if current := table.lease(object, identity); current != nil {
		// Joining a pending lease cannot change its captured state or deadline.
		if !current.Breaking {
			if grant.State != current.State {
				current.State = grant.State
				current.Epoch++
			}
		}
		return
	}
	grant.BreakTo = 0
	grant.Deadline = time.Time{}
	table.object(object).Leases = append(table.object(object).Leases, grant)
	table.leaseObjects[identity] = object
}

func (table *Table) releaseLeases(record *objectEntry, closed *openEntry) {
	before := len(record.Leases)
	record.Leases = slices.DeleteFunc(record.Leases, func(lease Lease) bool {
		for _, id := range record.Opens {
			open := table.opens[id]
			if open.ClientGUID == lease.ClientGUID && open.LeaseKey == lease.Key {
				return false
			}
		}
		delete(table.leaseObjects, leaseIdentity{client: lease.ClientGUID, key: lease.Key})
		return true
	})
	if len(record.Leases) != before || validBinding(closed.Binding) && closed.LeaseKey != (GUID{}) {
		table.signalBreakChanges()
	}
}

func (table *Table) leaseBinding(record *objectEntry, lease Lease) Binding {
	for _, id := range record.Opens {
		open := table.opens[id]
		if open.ClientGUID == lease.ClientGUID && open.LeaseKey == lease.Key && validBinding(open.Binding) {
			return open.Binding
		}
	}
	return Binding{}
}

// BreakLeases downgrades other leases and returns notifications and cleanup.
// The supplied client and key identify the requesting lease, which is excluded.
// When all members are detached and the break removes H, cleanup is immediate.
func (table *Table) BreakLeases(object smb.ObjectKey, clientGUID GUID, leaseKey GUID, target uint32) ([]Break, []CloseAction) {
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.breakLeasesLocked(object, clientGUID, leaseKey, target)
}

// breakLeasesLocked is the shared selection primitive; the caller holds mu.
func (table *Table) breakLeasesLocked(object smb.ObjectKey, clientGUID GUID, leaseKey GUID, target uint32) ([]Break, []CloseAction) {
	if !validLeaseState(target) && target != smb.LeaseRead|smb.LeaseWrite && target != smb.LeaseHandle {
		return nil, nil
	}
	record := table.objects[object]
	if record == nil {
		return nil, nil
	}
	var breaks []Break
	var detached []leaseRef
	queued := false
	for index := range record.Leases {
		lease := &record.Leases[index]
		if lease.ClientGUID == clientGUID && lease.Key == leaseKey {
			continue
		}
		newState := lease.State & target
		if lease.Breaking {
			if stronger := lease.queuedTo & newState; stronger != lease.queuedTo {
				lease.queuedTo = stronger
				queued = true
			}
			continue
		}
		if newState == lease.State {
			continue
		}
		binding := table.leaseBinding(record, *lease)
		if !validBinding(binding) && newState&smb.LeaseHandle == 0 {
			detached = append(detached, leaseRef{object: object, identity: leaseIdentity{client: lease.ClientGUID, key: lease.Key}})
			continue
		}
		lease.queuedTo = newState
		breaks = append(breaks, table.startLeaseBreak(lease, binding, newState, false))
	}
	if queued || len(breaks) != 0 {
		table.signalBreakChanges()
	}
	return breaks, table.revokeLeases(detached)
}

// startLeaseBreak runs under the table mutex. ACK continuations granting no W/H
// keep the chain epoch under MS-SMB2 3.3.4.7 Appendix A footnote 249.
func (table *Table) startLeaseBreak(lease *Lease, binding Binding, target uint32, continuation bool) Break {
	if !continuation || target&(smb.LeaseWrite|smb.LeaseHandle) != 0 {
		lease.Epoch++
	}
	ack := lease.State&(smb.LeaseHandle|smb.LeaseWrite) != 0
	notification := Break{
		Binding: binding, ClientGUID: lease.ClientGUID, LeaseKey: lease.Key,
		CurrentState: lease.State, NewState: target, Epoch: lease.Epoch, AckRequired: ack,
	}
	if notification.CurrentState&smb.LeaseRead != 0 && target&smb.LeaseRead == 0 {
		ticket := &readDeliveryTicket{currentState: notification.CurrentState, newState: target, epoch: notification.Epoch}
		lease.readDelivery, lease.readDelivered = ticket, false
		notification.readDelivery = ticket
	}
	lease.BreakTo, lease.Breaking, lease.Deadline = target, ack, time.Time{}
	if ack {
		lease.Deadline = table.now().Add(LeaseBreakTimeout)
	} else {
		lease.State = target
	}
	return notification
}

// AckBreak accepts only a pending break's identity and a subset of its target.
// Dropping H returns cleanup for any detached members of the lease. The caller
// validates the active session and any required tree before this transaction.
// Lease identity is ClientGUID and key, not the ACK session's open membership.
func (table *Table) AckBreak(binding Binding, clientGUID GUID, key GUID, leaseState uint32) ([]Break, []CloseAction, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	identity := leaseIdentity{client: clientGUID, key: key}
	if binding.SessionID == 0 {
		return nil, nil, smb.StatusInvalidParameter
	}
	object, exists := table.leaseObjects[identity]
	if !exists {
		return nil, nil, smb.StatusObjectNameNotFound
	}
	lease := table.lease(object, identity)
	if lease == nil {
		return nil, nil, smb.StatusObjectNameNotFound
	}
	if !lease.Breaking {
		return nil, nil, smb.StatusUnsuccessful
	}
	if leaseState & ^lease.BreakTo != 0 {
		return nil, nil, smb.StatusRequestNotAccepted
	}
	lease.State, lease.BreakTo, lease.Breaking, lease.Deadline = leaseState, leaseState, false, time.Time{}
	var notifications []Break
	if target := leaseState & lease.queuedTo; target != leaseState {
		// A queued destructive operation drops H first, then revokes R after
		// that acknowledgment. The R-only final notification needs no ACK.
		if leaseState&(smb.LeaseRead|smb.LeaseHandle) == smb.LeaseRead|smb.LeaseHandle && target&smb.LeaseHandle == 0 {
			target = leaseState &^ smb.LeaseHandle
		}
		notifications = append(notifications, table.startLeaseBreak(lease, table.leaseBinding(table.objects[object], *lease), target, true))
	}
	if !lease.Breaking {
		lease.queuedTo = 0
	}
	actions := table.dropDurability(object, identity, lease.State)
	table.signalBreakChanges()
	return notifications, actions, smb.StatusSuccess
}

func (table *Table) dropDurability(object smb.ObjectKey, identity leaseIdentity, state uint32) []CloseAction {
	if state&smb.LeaseHandle != 0 {
		return nil
	}
	var actions []CloseAction
	for _, id := range table.openIDs() {
		open := table.opens[id]
		if open.Object != object || open.ClientGUID != identity.client || open.LeaseKey != identity.key || !open.Durable {
			continue
		}
		if open.Binding.SessionID == 0 {
			actions = append(actions, table.closeOpen(open))
		} else {
			open.Durable = false
			open.DurableTimeout = 0
		}
	}
	return actions
}

type leaseRef struct {
	object   smb.ObjectKey
	identity leaseIdentity
}

// Closing members can remove leases, so callers capture references first.
func (table *Table) revokeLeases(leases []leaseRef) []CloseAction {
	var actions []CloseAction
	for _, ref := range leases {
		lease := table.lease(ref.object, ref.identity)
		if lease == nil {
			continue
		}
		lease.State, lease.BreakTo, lease.queuedTo, lease.Breaking, lease.Deadline = 0, 0, 0, false, time.Time{}
		lease.resetReadRevocation()
		table.signalBreakChanges()
		actions = append(actions, table.dropDurability(ref.object, ref.identity, 0)...)
	}
	return actions
}

func (table *Table) closeDetachedBreaks() []CloseAction {
	var detached []leaseRef
	for object, record := range table.objects {
		for _, lease := range record.Leases {
			if lease.Breaking && lease.BreakTo&smb.LeaseHandle == 0 && !validBinding(table.leaseBinding(record, lease)) {
				detached = append(detached, leaseRef{object: object, identity: leaseIdentity{client: lease.ClientGUID, key: lease.Key}})
			}
		}
	}
	return table.revokeLeases(detached)
}

// ExpireBreaks revokes the whole lease when an acknowledgment times out.
// Attached opens lose durability but remain usable; detached opens close.
func (table *Table) ExpireBreaks() []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	now := table.now()
	var expired []leaseRef
	for object, record := range table.objects {
		for _, lease := range record.Leases {
			if lease.Breaking && !lease.Deadline.After(now) {
				expired = append(expired, leaseRef{object: object, identity: leaseIdentity{client: lease.ClientGUID, key: lease.Key}})
			}
		}
	}
	return table.revokeLeases(expired)
}
