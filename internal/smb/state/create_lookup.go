package state

import "github.com/djosh34/s3-smb/internal/smb"

// LookupCreate finds the original open by client, user, share and CreateGUID.
// A reserved identity returns DUPLICATE_OBJECTID. A snapshot is not permission
// to use the open: Replay must still validate the complete request and binding.
func (table *Table) LookupCreate(request OpenRequest) (Open, smb.Status) {
	table.mu.Lock()
	defer table.mu.Unlock()
	entry, exists := table.creates[identity(request)]
	if !exists {
		return Open{}, smb.StatusObjectNameNotFound
	}
	if entry.reservation != 0 {
		return Open{}, smb.StatusDuplicateObjectID
	}
	return table.opens[entry.persistent].Open, smb.StatusSuccess
}
