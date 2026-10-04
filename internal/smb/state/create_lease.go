package state

import "github.com/djosh34/s3-smb/internal/smb"

// LeaseFor returns a copy of the lease for this object, client and key.
// Its state and epoch include any pending break. Missing leases return false.
func (table *Table) LeaseFor(object smb.ObjectKey, clientGUID, key GUID) (Lease, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	lease := table.lease(object, leaseIdentity{client: clientGUID, key: key})
	if lease == nil {
		return Lease{}, false
	}
	return *lease, true
}

// LeasesNeedBreak reports other leases that exceed the target or have a pending
// break. The exact client and key are excluded, as in BreakLeases.
func (table *Table) LeasesNeedBreak(object smb.ObjectKey, clientGUID, key GUID, target uint32) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	record := table.objects[object]
	if record == nil {
		return false
	}
	for _, lease := range record.Leases {
		if lease.ClientGUID == clientGUID && lease.Key == key {
			continue
		}
		if lease.Breaking || lease.State & ^target != 0 {
			return true
		}
	}
	return false
}

// PrepareLease selects safe caching rights for a live CREATE reservation.
// It does not publish a grant. Commit checks it again. Other opens or reservations
// remove W, and another writer removes R and H. A shared lease is never demoted
// by a smaller request or promoted during a break (MS-SMB2 3.3.5.9.11).
func (table *Table) PrepareLease(reservation Reservation, requested Lease) (Lease, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	request, exists := table.reservations[reservation]
	if !exists || !validLeaseState(requested.State) || requested.Breaking {
		return Lease{}, smb.StatusInvalidParameter
	}
	if requested.State == 0 {
		return Lease{}, smb.StatusSuccess
	}
	if requested.Key == (GUID{}) || requested.ClientGUID != request.ClientGUID || request.Object.Stream != "" {
		return Lease{}, smb.StatusInvalidParameter
	}
	identity := leaseIdentity{client: requested.ClientGUID, key: requested.Key}
	if object, found := table.leaseObjects[identity]; found && object != request.Object {
		return Lease{}, smb.StatusInvalidParameter
	}
	selected := requested
	for _, open := range table.opens {
		if open.Object == request.Object && (open.ClientGUID != requested.ClientGUID || open.LeaseKey != requested.Key) {
			selected.State = reduceLease(selected.State, open.SharingIntent)
		}
	}
	for token, reserved := range table.reservations {
		if token != reservation && reserved.Object == request.Object {
			selected.State = reduceLease(selected.State, reserved.SharingIntent)
		}
	}
	if current := table.lease(request.Object, identity); current != nil {
		selected.Epoch, selected.ParentKey = current.Epoch, current.ParentKey
		if current.Breaking {
			selected.State &= current.BreakTo
		} else if current.State & ^requested.State != 0 {
			selected.State = current.State
		}
		if !validLeaseState(selected.State) {
			selected.State = 0
		}
	} else {
		// Acquiring the initial nonzero V2 state is a state change.
		selected.Epoch++
	}
	if selected.State == 0 {
		return Lease{}, smb.StatusSuccess
	}
	return selected, smb.StatusSuccess
}

func reduceLease(leaseState uint32, rights Rights) uint32 {
	if rights&RightWrite != 0 {
		return 0
	}
	return leaseState &^ smb.LeaseWrite
}

// SharingHandleLeases returns objects whose conflicting opens hold H leases.
// A reservation or a conflicting open without H does not cause a lease retry.
// Base-file deletion can conflict with an unnamed-file lease from a stream open.
func (table *Table) SharingHandleLeases(request OpenRequest) []smb.ObjectKey {
	table.mu.Lock()
	defer table.mu.Unlock()
	request = sharingIntent(request)
	objects := make(map[smb.ObjectKey]bool)
	var result []smb.ObjectKey
	for _, open := range table.opens {
		if sharingCompatible(request, openRequest(open.Open)) {
			continue
		}
		lease := table.lease(open.Object, leaseIdentity{client: open.ClientGUID, key: open.LeaseKey})
		if lease != nil && lease.State&smb.LeaseHandle != 0 && !objects[open.Object] {
			objects[open.Object] = true
			result = append(result, open.Object)
		}
	}
	return result
}
