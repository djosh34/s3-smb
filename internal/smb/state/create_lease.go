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

// LeaseForOpen validates the complete current ID/binding and returns an atomic
// open and shared-lease snapshot. Both are immutable copies. An unleased open
// returns an empty lease; a missing associated lease returns OBJECT_NAME_NOT_FOUND.
// Callers project effective H through the captured pending target when deciding
// whether to advertise durability. The snapshot itself changes no retained state.
func (table *Table) LeaseForOpen(id FileID, binding Binding) (Open, Lease, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return Open{}, Lease{}, status
	}
	if open.LeaseKey == (GUID{}) {
		return open.Open, Lease{}, smb.StatusSuccess
	}
	current := table.lease(open.Object, leaseIdentity{client: open.ClientGUID, key: open.LeaseKey})
	if current == nil {
		return Open{}, Lease{}, smb.StatusObjectNameNotFound
	}
	return open.Open, *current, smb.StatusSuccess
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
// remove W; writable access alone does not remove R/H. A shared lease is never demoted
// by a smaller request or promoted during a break (MS-SMB2 3.3.5.9.11).
// A zero State with an existing Key keeps membership without changing the lease;
// LeaseFor supplies its held state, including H after an acknowledged downgrade.
func (table *Table) PrepareLease(reservation Reservation, requested Lease) (Lease, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.prepareLease(reservation, requested)
}

// prepareLease requires table.mu; selection and publication can share one lock.
func (table *Table) prepareLease(reservation Reservation, requested Lease) (Lease, smb.Status) {
	request, exists := table.reservations[reservation]
	if !exists || !validLeaseState(requested.State) || requested.Breaking {
		return Lease{}, smb.StatusInvalidParameter
	}
	if requested.State == 0 && requested.Key == (GUID{}) {
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
	joining := false
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
		joining = true
		selected.Epoch, selected.ParentKey = current.Epoch, current.ParentKey
		if current.Breaking {
			// Join without acquisition. The shared held state, captured
			// target, epoch and deadline remain owned by the active break.
			selected.State = 0
		} else if current.State & ^requested.State != 0 || current.State & ^selected.State != 0 || selected.State == current.State {
			// An unchanged join acquires nothing. A zero-state membership
			// cannot resurrect captured rights if a break precedes Commit.
			selected.State = 0
		}
		if !validLeaseState(selected.State) {
			selected.State = 0
		}
	} else {
		// Acquiring the initial nonzero V2 state is a state change.
		selected.Epoch++
	}
	if selected.State != 0 && (!table.leasesAllow(request, selected) || !table.leaseMutationAllows(request.Object, selected.ClientGUID, selected.Key)) {
		// Neither competing W nor an in-flight mutation permits fresh
		// caching. Zero-state shared membership remains unchanged.
		selected.State = 0
	}
	if selected.State == 0 && !joining {
		return Lease{}, smb.StatusSuccess
	}
	return selected, smb.StatusSuccess
}

// CommitLease reselects a prepared CREATE lease and publishes the open atomically.
// Storage and lease metadata are validated before adjusting the proposal. A valid
// granted timeout is declined if effective H was lost; invalid grants remain
// reserved. All other Grant fields pass through unchanged. No storage IO is done.
func (table *Table) CommitLease(reservation Reservation, grant Grant, requested Lease) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	request, exists := table.reservations[reservation]
	if !exists {
		return Open{}, smb.StatusInvalidParameter
	}
	if table.deletePending(request.Object) {
		return Open{}, smb.StatusDeletePending
	}
	if status := table.validateGrantMetadata(request, reservation, grant); status != smb.StatusSuccess {
		return Open{}, status
	}
	if status := table.validateLeaseMetadata(request, grant); status != smb.StatusSuccess {
		return Open{}, status
	}
	if grant.Lease.Key != (GUID{}) && (grant.Lease.Key != requested.Key || grant.Lease.ClientGUID != requested.ClientGUID) {
		return Open{}, smb.StatusInvalidParameter
	}
	if grant.DurableTimeout != 0 && request.CreateGUID == (GUID{}) {
		return Open{}, smb.StatusInvalidParameter
	}
	selected, status := table.prepareLease(reservation, requested)
	if status != smb.StatusSuccess {
		return Open{}, status
	}
	grant.Lease = selected
	if grant.DurableTimeout != 0 && !table.durableLeaseEligible(request, selected) {
		grant.DurableTimeout = 0
	}
	return table.commitOpen(reservation, grant)
}

func reduceLease(leaseState uint32, _ Rights) uint32 {
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
