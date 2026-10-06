package state

import (
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// LeaseBreakTimeout bounds an unacknowledged break (MS-SMB2 3.3.2.5).
const LeaseBreakTimeout = 35 * time.Second

// handle reports whether the lease keeps H, counting a pending break as done.
func (lease *Lease) handle() bool {
	if lease.Breaking {
		return lease.BreakTo&smb.LeaseHandle != 0
	}
	return lease.State&smb.LeaseHandle != 0
}

func (lease *Lease) is(clientGUID, key GUID) bool {
	return lease.ClientGUID == clientGUID && lease.Key == key
}

// otherLease returns the lease on object unless it belongs to clientGUID and key.
func (table *Table) otherLease(object smb.Inode, clientGUID, key GUID) *Lease {
	record := table.objects[object]
	if record == nil || record.lease == nil || record.lease.is(clientGUID, key) {
		return nil
	}
	return record.lease
}

// LeaseKeyElsewhere reports whether the lease key of clientGUID names a file
// other than object. A CREATE checks it before it changes storage; Commit
// checks it again for a CREATE that raced another one with the same key.
func (table *Table) LeaseKeyElsewhere(object smb.Inode, clientGUID, key GUID) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.leaseKeyElsewhere(object, leaseIdentity{client: clientGUID, key: key})
}

// leaseKeyElsewhere requires mu.
func (table *Table) leaseKeyElsewhere(object smb.Inode, identity leaseIdentity) bool {
	held, exists := table.leaseObjects[identity]
	return exists && held != object
}

// grantLease gives the new open of request the requested lease, joins the
// existing lease of the same key, or gives it none. It returns the lease key
// the open joins. W needs every other open of the file to share the lease.
// A held lease only grows, and not while it is breaking. Requires mu.
func (table *Table) grantLease(request OpenRequest, reservation Reservation, requested Lease) (GUID, smb.Status) {
	if requested.Key == (GUID{}) {
		return GUID{}, smb.StatusSuccess
	}
	identity := leaseIdentity{client: requested.ClientGUID, key: requested.Key}
	if table.leaseKeyElsewhere(request.Object, identity) {
		return GUID{}, smb.StatusInvalidParameter
	}
	record := table.object(request.Object)
	current := record.lease
	if current != nil && !current.is(requested.ClientGUID, requested.Key) {
		return GUID{}, smb.StatusSuccess
	}
	allowed := requested.State
	if !table.onlyLeaseOpens(request.Object, reservation, identity) {
		allowed &^= smb.LeaseWrite
	}
	if current == nil {
		if allowed == 0 {
			return GUID{}, smb.StatusSuccess
		}
		lease := Lease{ClientGUID: requested.ClientGUID, Key: requested.Key, ParentKey: requested.ParentKey, State: allowed, Epoch: requested.Epoch + 1}
		record.lease = &lease
		table.leaseObjects[identity] = request.Object
		return requested.Key, smb.StatusSuccess
	}
	if !current.Breaking && allowed&current.State == current.State && allowed != current.State {
		current.State = allowed
		current.Epoch++
	}
	return requested.Key, smb.StatusSuccess
}

// onlyLeaseOpens reports whether every other open and reservation of object
// belongs to the lease identity. Requires mu.
func (table *Table) onlyLeaseOpens(object smb.Inode, reservation Reservation, identity leaseIdentity) bool {
	for _, id := range table.objects[object].Opens {
		open := table.opens[id]
		if open.ClientGUID != identity.client || open.LeaseKey != identity.key {
			return false
		}
	}
	for token, reserved := range table.reservations {
		if token != reservation && reserved.Object == object {
			return false
		}
	}
	return true
}

// releaseLease drops the lease once no open of its key is left. Requires mu.
func (table *Table) releaseLease(record *objectEntry) {
	lease := record.lease
	if lease == nil {
		return
	}
	for _, id := range record.Opens {
		if table.opens[id].LeaseKey == lease.Key {
			return
		}
	}
	record.lease = nil
	delete(table.leaseObjects, leaseIdentity{client: lease.ClientGUID, key: lease.Key})
	table.signalBreakChanges()
}

// leaseBinding returns the binding of an attached open of the lease, or zero.
func (table *Table) leaseBinding(record *objectEntry) Binding {
	for _, id := range record.Opens {
		open := table.opens[id]
		if open.LeaseKey == record.lease.Key && validBinding(open.Binding) {
			return open.Binding
		}
	}
	return Binding{}
}

// BreakLease starts breaking the lease on object down to target, unless the
// lease belongs to clientGUID and key, already fits target or is breaking.
// It reports the notification to send, if any. Losing only R needs no
// acknowledgment. A lease whose opens are all detached has nobody to tell, so
// it drops to target at once. Opens that lose H stop being durable; detached
// ones are closed and returned for cleanup.
func (table *Table) BreakLease(object smb.Inode, clientGUID, key GUID, target uint32) (Break, bool, []CloseAction) {
	table.mu.Lock()
	defer table.mu.Unlock()
	lease := table.otherLease(object, clientGUID, key)
	if lease == nil || lease.Breaking || lease.State&^target == 0 {
		return Break{}, false, nil
	}
	newState := lease.State & target
	lease.Epoch++
	binding := table.leaseBinding(table.objects[object])
	notification := Break{
		Binding: binding, ClientGUID: lease.ClientGUID, LeaseKey: lease.Key,
		CurrentState: lease.State, NewState: newState, Epoch: lease.Epoch,
		AckRequired: lease.State&(smb.LeaseWrite|smb.LeaseHandle) != 0,
	}
	notify := validBinding(binding)
	if notify && notification.AckRequired {
		lease.Breaking, lease.BreakTo, lease.Deadline = true, newState, table.now().Add(LeaseBreakTimeout)
	} else {
		lease.State = newState
	}
	table.signalBreakChanges()
	return notification, notify, table.dropDurability(object, lease)
}

// AckBreak accepts the acknowledgment of a pending break to a subset of its
// target. Opens that lose H stop being durable; detached ones are closed.
func (table *Table) AckBreak(clientGUID, key GUID, leaseState uint32) ([]CloseAction, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	object, exists := table.leaseObjects[leaseIdentity{client: clientGUID, key: key}]
	if !exists {
		return nil, smb.StatusObjectNameNotFound
	}
	lease := table.objects[object].lease
	if !lease.Breaking {
		return nil, smb.StatusUnsuccessful
	}
	if leaseState&^lease.BreakTo != 0 {
		return nil, smb.StatusRequestNotAccepted
	}
	lease.State, lease.BreakTo, lease.Breaking, lease.Deadline = leaseState, 0, false, time.Time{}
	table.signalBreakChanges()
	return table.dropDurability(object, lease), smb.StatusSuccess
}

// ExpireBreaks revokes every lease whose break was not acknowledged in time.
// Attached opens lose durability but stay usable; detached opens close.
func (table *Table) ExpireBreaks() []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	now := table.now()
	var expired []smb.Inode
	for object, record := range table.objects {
		if lease := record.lease; lease != nil && lease.Breaking && !lease.Deadline.After(now) {
			expired = append(expired, object)
		}
	}
	var actions []CloseAction
	for _, object := range expired {
		lease := table.objects[object].lease
		lease.State, lease.BreakTo, lease.Breaking, lease.Deadline = 0, 0, false, time.Time{}
		table.signalBreakChanges()
		actions = append(actions, table.dropDurability(object, lease)...)
	}
	return actions
}

// dropDurability ends durability of the lease's opens once it loses H.
// Detached opens close, because nothing could reconnect them. Requires mu.
func (table *Table) dropDurability(object smb.Inode, lease *Lease) []CloseAction {
	if lease.handle() {
		return nil
	}
	var actions []CloseAction
	for _, id := range table.openIDs() {
		open := table.opens[id]
		if open.Object != object || open.LeaseKey != lease.Key || !open.Durable {
			continue
		}
		if open.Binding.SessionID == 0 {
			actions = append(actions, table.closeOpen(open))
		} else {
			open.Durable, open.DurableTimeout = false, 0
		}
	}
	return actions
}

// LeaseNeedsBreak reports whether the lease on object, unless it belongs to
// clientGUID and key, holds rights outside target or is breaking.
func (table *Table) LeaseNeedsBreak(object smb.Inode, clientGUID, key GUID, target uint32) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	lease := table.otherLease(object, clientGUID, key)
	return lease != nil && (lease.Breaking || lease.State&^target != 0)
}

// LeaseBreaking reports whether the lease on object, unless it belongs to
// clientGUID and key, waits for an acknowledgment.
func (table *Table) LeaseBreaking(object smb.Inode, clientGUID, key GUID) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	lease := table.otherLease(object, clientGUID, key)
	return lease != nil && lease.Breaking
}

// BreakChanges returns a channel closed at the next change of a lease's
// breaking state or removal. Fetch it before checking LeaseBreaking.
func (table *Table) BreakChanges() <-chan struct{} {
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.breakChanges
}

func (table *Table) signalBreakChanges() {
	close(table.breakChanges)
	table.breakChanges = make(chan struct{})
}

// SharingLease reports the file whose H lease, unless it belongs to
// clientGUID and key, holds an open that conflicts with request's sharing.
// Breaking H lets the client close cached handles; the opener's own lease is
// never broken.
func (table *Table) SharingLease(request OpenRequest, clientGUID, key GUID) (smb.Inode, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	request = sharingIntent(request)
	for _, open := range table.opens {
		if sharingCompatible(request, openRequest(open.Open)) || open.LeaseKey == (GUID{}) {
			continue
		}
		if lease := table.objects[open.Object].lease; !lease.is(clientGUID, key) && lease.State&smb.LeaseHandle != 0 {
			return open.Object, true
		}
	}
	return 0, false
}

// LeaseForOpen finds the open like Find and returns it with a copy of its
// lease, or a zero lease for an open without one.
func (table *Table) LeaseForOpen(id FileID, binding Binding) (Open, Lease, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return Open{}, Lease{}, status
	}
	var lease Lease
	if current := table.objects[open.Object].lease; current != nil && open.LeaseKey != (GUID{}) {
		lease = *current
	}
	return open.Open, lease, smb.StatusSuccess
}
