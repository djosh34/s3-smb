package state

import "github.com/djosh34/s3-smb/internal/smb"

// Each allocation identifies one captured revocation, even across identical wire
// epochs and recreated leases. This immutable ticket has nonzero size.
type readDeliveryTicket struct {
	currentState uint32
	newState     uint32
	epoch        uint16
}

// resetReadRevocation runs under mu on actual new READ authority, not joins.
func (lease *Lease) resetReadRevocation() {
	lease.readDelivery, lease.readDelivered = nil, false
}

// readRevocationComplete is a pure value query for mutation readiness under mu.
// Pending no-ACK delivery remains an obligation after held State becomes NONE.
func (lease Lease) readRevocationComplete() bool {
	if lease.readDelivery != nil {
		return lease.readDelivered
	}
	return lease.State&smb.LeaseRead == 0
}

// readDeliveryLease requires mu and matches the opaque captured identity before
// checking its immutable wire fields. Epoch/key equality alone proves nothing.
func (table *Table) readDeliveryLease(notification Break) *Lease {
	ticket := notification.readDelivery
	if ticket == nil {
		return nil
	}
	identity := leaseIdentity{client: notification.ClientGUID, key: notification.LeaseKey}
	object, exists := table.leaseObjects[identity]
	if !exists {
		return nil
	}
	lease := table.lease(object, identity)
	if lease == nil || lease.readDelivery != ticket || ticket.currentState != notification.CurrentState || ticket.newState != notification.NewState || ticket.epoch != notification.Epoch {
		return nil
	}
	return lease
}

// NoteLeaseBreakDelivered records only a successful real sender completion.
// The caller invokes it outside identity locks; no protocol state is changed.
func (table *Table) NoteLeaseBreakDelivered(notification Break) {
	table.mu.Lock()
	defer table.mu.Unlock()
	lease := table.readDeliveryLease(notification)
	if lease == nil || lease.readDelivered {
		return
	}
	lease.readDelivered = true
	table.signalBreakChanges()
}
