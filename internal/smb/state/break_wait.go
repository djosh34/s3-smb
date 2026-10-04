package state

import "github.com/djosh34/s3-smb/internal/smb"

// BreakChanges returns a broadcast channel closed when lease break state changes.
// Capture the channel before testing pending state to avoid missing a change.
// Each change replaces the channel, so callers fetch it again after waking.
func (table *Table) BreakChanges() <-chan struct{} {
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.breakChanges
}

func (table *Table) signalBreakChanges() {
	close(table.breakChanges)
	table.breakChanges = make(chan struct{})
}

// BreakPending reports whether the lease still needs an acknowledgment. A later
// downgrade of the same lease remains pending until acknowledged or revoked.
func (table *Table) BreakPending(notification Break) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	identity := leaseIdentity{client: notification.ClientGUID, key: notification.LeaseKey}
	object, exists := table.leaseObjects[identity]
	if !exists {
		return false
	}
	lease := table.lease(object, identity)
	return lease != nil && lease.Breaking
}

// LeasesBreaking includes already pending breaks on the object, excluding the
// requesting lease. A concurrent CREATE must also wait for an earlier break.
func (table *Table) LeasesBreaking(object smb.ObjectKey, clientGUID, leaseKey GUID) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	record := table.objects[object]
	if record == nil {
		return false
	}
	for _, lease := range record.Leases {
		if lease.Breaking && (lease.ClientGUID != clientGUID || lease.Key != leaseKey) {
			return true
		}
	}
	return false
}

// CompleteDetachedBreaks completes all fully detached H-preserving breaks on
// the object, including earlier pending breaks. The requesting lease is excluded.
func (table *Table) CompleteDetachedBreaks(object smb.ObjectKey, clientGUID, leaseKey GUID) []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	record := table.objects[object]
	if record == nil {
		return nil
	}
	var detached []leaseRef
	for _, lease := range record.Leases {
		if lease.ClientGUID == clientGUID && lease.Key == leaseKey {
			continue
		}
		if lease.Breaking && lease.BreakTo&smb.LeaseHandle != 0 && !validBinding(table.leaseBinding(record, lease)) {
			detached = append(detached, leaseRef{object: object, identity: leaseIdentity{client: lease.ClientGUID, key: lease.Key}})
		}
	}
	return table.revokeLeases(detached)
}

// CompleteDetachedBreak completes a captured break as NONE if it retains H and
// every member is still detached. Reattachment or a later break leaves it alone.
// This is the server's no-connection path from MS-SMB2 3.3.4.7.
func (table *Table) CompleteDetachedBreak(notification Break) []CloseAction {
	table.mu.Lock()
	defer table.mu.Unlock()
	identity := leaseIdentity{client: notification.ClientGUID, key: notification.LeaseKey}
	object, exists := table.leaseObjects[identity]
	if !exists {
		return nil
	}
	lease := table.lease(object, identity)
	if lease == nil || !lease.Breaking || lease.Epoch != notification.Epoch || lease.BreakTo&smb.LeaseHandle == 0 || validBinding(table.leaseBinding(table.objects[object], *lease)) {
		return nil
	}
	return table.revokeLeases([]leaseRef{{object: object, identity: identity}})
}
