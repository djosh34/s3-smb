package state

import "github.com/djosh34/s3-smb/internal/smb"

// DurableEligible reports whether this reservation's proposed lease will retain
// H. Zero selected rights can still join an existing shared lease. A break that
// removes H, including a queued revocation, forbids durability even while the
// captured response still reports H.
// This is a snapshot; Commit repeats the check under its own table lock.
func (table *Table) DurableEligible(reservation Reservation, lease Lease) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	request, exists := table.reservations[reservation]
	return exists && table.durableLeaseEligible(request, lease)
}

func (table *Table) durableLeaseEligible(request OpenRequest, proposed Lease) bool {
	if proposed.Key == (GUID{}) || proposed.ClientGUID != request.ClientGUID {
		return false
	}
	rights := proposed.State
	if current := table.lease(request.Object, leaseIdentity{client: proposed.ClientGUID, key: proposed.Key}); current != nil {
		if current.Breaking || rights == 0 {
			rights = current.EffectiveState()
		}
	}
	return rights&smb.LeaseHandle != 0
}
