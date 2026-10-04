package state

import "github.com/djosh34/s3-smb/internal/smb"

// LookupCreate finds the open by client, user, share and CreateGUID. A
// CREATE still in progress with that identity gives DUPLICATE_OBJECTID.
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
