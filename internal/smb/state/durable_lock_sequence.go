package state

import "github.com/djosh34/s3-smb/internal/smb"

type lockSequenceEntry struct {
	number uint8
	valid  bool
}

// LockSequence applies durable LOCK replay rules from MS-SMB2 3.3.5.14.
// Sequence packs a 28-bit index and a 4-bit number. Indices outside 1..64 and
// ordinary opens use normal vector processing. A matching valid number succeeds
// without inspecting the ranges; the protocol does not compare request bodies.
// A different number invalidates the old entry even if the atomic vector fails.
// Verification, range publication and successful history updates share one lock.
func (table *Table) LockSequence(id FileID, binding Binding, ranges []Range, unlock bool, sequence uint32) smb.Status {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return status
	}
	index := sequence >> 4
	if !open.Durable || index == 0 || index > uint32(len(open.lockSequences)) {
		return table.applyLocks(open, ranges, unlock)
	}
	entry := &open.lockSequences[index-1]
	number := uint8(sequence & 15)
	if entry.valid && entry.number == number {
		return smb.StatusSuccess
	}
	entry.valid = false
	status = table.applyLocks(open, ranges, unlock)
	if status == smb.StatusSuccess {
		*entry = lockSequenceEntry{number: number, valid: true}
	}
	return status
}
