package state

import (
	"slices"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// LeaseBreakTimeout bounds an unacknowledged break, as in MS-SMB2.
const LeaseBreakTimeout = 35 * time.Second

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
	if lease.State == 0 {
		return smb.StatusSuccess
	}
	if grant.Directory || request.Object.Stream != "" || lease.Key == (GUID{}) || lease.ClientGUID != request.ClientGUID {
		return smb.StatusInvalidParameter
	}
	identity := leaseIdentity{client: lease.ClientGUID, key: lease.Key}
	if object, exists := table.leaseObjects[identity]; exists && object != request.Object {
		return smb.StatusInvalidParameter
	}
	if current := table.lease(request.Object, identity); current != nil {
		if (current.Breaking && lease.State & ^current.BreakTo != 0) || (!current.Breaking && current.State & ^lease.State != 0) {
			return smb.StatusInvalidParameter
		}
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

func (table *Table) leasesAllow(request OpenRequest, joining Lease) bool {
	record := table.objects[request.Object]
	if record == nil {
		return true
	}
	for _, held := range record.Leases {
		if held.ClientGUID == joining.ClientGUID && held.Key == joining.Key && joining.State != 0 {
			continue
		}
		if !leaseCompatible(held.State, request.SharingIntent) {
			return false
		}
	}
	return true
}

func leaseCompatible(state uint32, rights Rights) bool {
	return state&smb.LeaseWrite == 0 && (state&smb.LeaseRead == 0 || rights&RightWrite == 0)
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

func (table *Table) releaseLeases(record *objectEntry) {
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

// BreakLeases downgrades other leases and returns captured notifications.
// The supplied client and key identify the requesting lease, which is excluded.
func (table *Table) BreakLeases(object smb.ObjectKey, clientGUID GUID, leaseKey GUID, target uint32) []Break {
	table.mu.Lock()
	defer table.mu.Unlock()
	if !validLeaseState(target) {
		return nil
	}
	record := table.objects[object]
	if record == nil {
		return nil
	}
	var breaks []Break
	for index := range record.Leases {
		lease := &record.Leases[index]
		if lease.ClientGUID == clientGUID && lease.Key == leaseKey {
			continue
		}
		newState := lease.State & target
		if newState == lease.State || (lease.Breaking && lease.BreakTo & ^newState == 0) {
			continue
		}
		if lease.Breaking {
			newState &= lease.BreakTo
		}
		lease.Epoch++
		ack := lease.State&(smb.LeaseHandle|smb.LeaseWrite) != 0
		breaks = append(breaks, Break{
			Binding: table.leaseBinding(record, *lease), ClientGUID: lease.ClientGUID, LeaseKey: lease.Key,
			CurrentState: lease.State, NewState: newState, Epoch: lease.Epoch, AckRequired: ack,
		})
		lease.BreakTo = newState
		lease.Breaking = ack
		if ack {
			lease.Deadline = table.now().Add(LeaseBreakTimeout)
		} else {
			lease.State = newState
			lease.Deadline = time.Time{}
		}
	}
	return breaks
}

// AckBreak accepts only a pending break's identity and a subset of its target.
// Dropping H returns cleanup for any detached members of the lease.
func (table *Table) AckBreak(binding Binding, clientGUID GUID, key GUID, leaseState uint32) ([]CloseAction, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	identity := leaseIdentity{client: clientGUID, key: key}
	object, exists := table.leaseObjects[identity]
	if !exists || !validBinding(binding) {
		return nil, smb.StatusInvalidParameter
	}
	lease := table.lease(object, identity)
	if lease == nil || !lease.Breaking || leaseState & ^lease.BreakTo != 0 || !table.ownsLease(binding, object, identity) {
		return nil, smb.StatusInvalidParameter
	}
	lease.State, lease.BreakTo, lease.Breaking, lease.Deadline = leaseState, leaseState, false, time.Time{}
	return table.dropDurability(object, identity, leaseState), smb.StatusSuccess
}

func (table *Table) ownsLease(binding Binding, object smb.ObjectKey, identity leaseIdentity) bool {
	for _, id := range table.objects[object].Opens {
		open := table.opens[id]
		if open.Binding == binding && open.ClientGUID == identity.client && open.LeaseKey == identity.key {
			return true
		}
	}
	return false
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

// ExpireBreaks applies timed-out targets and releases detached opens losing H.
func (table *Table) ExpireBreaks() []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	now := table.now()
	// Closing a detached member can remove its lease, so capture transitions first.
	type expired struct {
		object   smb.ObjectKey
		identity leaseIdentity
		state    uint32
	}
	var expiredLeases []expired
	for object, record := range table.objects {
		for index := range record.Leases {
			lease := &record.Leases[index]
			if !lease.Breaking || lease.Deadline.After(now) {
				continue
			}
			lease.State, lease.Breaking, lease.Deadline = lease.BreakTo, false, time.Time{}
			expiredLeases = append(expiredLeases, expired{object: object, identity: leaseIdentity{client: lease.ClientGUID, key: lease.Key}, state: lease.State})
		}
	}
	var actions []CloseAction
	for _, lease := range expiredLeases {
		actions = append(actions, table.dropDurability(lease.object, lease.identity, lease.state)...)
	}
	return actions
}
