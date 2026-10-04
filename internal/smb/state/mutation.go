package state

import "github.com/djosh34/s3-smb/internal/smb"

// Mutation reserves lease-grant exclusion through a caller's actual operation.
// Tokens are independent of open lifetime and are released with EndMutation.
// Storage references and cancellation remain the server caller's responsibility.
type Mutation uint64

type mutationEntry struct {
	object   smb.ObjectKey
	identity leaseIdentity
}

// BeginMutation validates the complete open identity, reserves grant exclusion
// and selects competing R/W invalidations in one transaction. Callers deliver
// returned work and wait only for held W, never under the table mutex.
func (table *Table) BeginMutation(id FileID, binding Binding) (Mutation, []Break, []CloseAction, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return 0, nil, nil, status
	}
	if !availableID(table.nextMutation) {
		return 0, nil, nil, smb.StatusInsufficientResources
	}
	table.nextMutation++
	mutation := Mutation(table.nextMutation)
	table.mutations[mutation] = mutationEntry{
		object:   open.Object,
		identity: leaseIdentity{client: open.ClientGUID, key: open.LeaseKey},
	}
	notifications, actions := table.breakLeasesLocked(open.Object, open.ClientGUID, open.LeaseKey, 0)
	return mutation, notifications, actions, smb.StatusSuccess
}

// EndMutation releases one token, including after its open closes or detaches.
// Zero and consumed tokens are harmless; one release never ends another token.
func (table *Table) EndMutation(mutation Mutation) {
	table.mu.Lock()
	defer table.mu.Unlock()
	entry, exists := table.mutations[mutation]
	if !exists {
		return
	}
	delete(table.mutations, mutation)
	table.prune(entry.object)
}

// leaseMutationAllows requires table.mu. Only the same ClientGUID/key is exempt;
// new competing grants or promotions must not race a break or actual mutation.
// It does not change an existing lease's held state or its membership.
func (table *Table) leaseMutationAllows(object smb.ObjectKey, clientGUID, key GUID) bool {
	identity := leaseIdentity{client: clientGUID, key: key}
	for _, mutation := range table.mutations {
		if mutation.object == object && mutation.identity != identity {
			return false
		}
	}
	return true
}

// MutationReady requires both competing W flush and delivered READ revocation.
// A queued target or held-State change is not delivery proof. The G2-owned
// receipt tracks actual captured notification emission independently of H ACK.
// Unknown or ended tokens are not ready.
func (table *Table) MutationReady(mutation Mutation) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	entry, exists := table.mutations[mutation]
	if !exists {
		return false
	}
	record := table.objects[entry.object]
	if record == nil {
		return true
	}
	for _, lease := range record.Leases {
		if lease.ClientGUID == entry.identity.client && lease.Key == entry.identity.key {
			continue
		}
		if lease.State&smb.LeaseWrite != 0 || !lease.readRevocationComplete() {
			return false
		}
	}
	return true
}

// MutationNeedsWait reports competing HELD write caching, even while a pending
// target or EffectiveState has already removed W. R/RH notification ACKs alone
// do not delay R/W mutation operations under MS-FSA 2.1.4.12. Only live tokens
// may be queried; ended or unknown tokens return false.
func (table *Table) MutationNeedsWait(mutation Mutation) bool {
	table.mu.Lock()
	defer table.mu.Unlock()
	entry, exists := table.mutations[mutation]
	if !exists {
		return false
	}
	record := table.objects[entry.object]
	if record == nil {
		return false
	}
	for _, lease := range record.Leases {
		if (lease.ClientGUID != entry.identity.client || lease.Key != entry.identity.key) && lease.State&smb.LeaseWrite != 0 {
			return true
		}
	}
	return false
}
