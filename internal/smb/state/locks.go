package state

import (
	"math"
	"slices"

	"github.com/djosh34/s3-smb/internal/smb"
)

func validRange(offset, length uint64) bool {
	return length == 0 || length-1 <= math.MaxUint64-offset
}

// overlaps follows MS-FSA 2.1.4.10, including its zero-byte rule. {0, 0}
// never overlaps. A zero-byte range at another offset overlaps a range only
// when the offset is strictly inside it, not at either boundary.
func overlaps(left, right Range) bool {
	if (left.Offset == 0 && left.Length == 0) || (right.Offset == 0 && right.Length == 0) {
		return false
	}
	return left.Offset <= lastByte(right) && right.Offset <= lastByte(left)
}

func lastByte(lock Range) uint64 {
	if lock.Length == 0 {
		return lock.Offset - 1
	}
	return lock.Offset + (lock.Length - 1)
}

func lockConflict(request, held Range) bool {
	if !overlaps(request, held) {
		return false
	}
	if held.Exclusive {
		return request.Owner != held.Owner || request.Exclusive
	}
	return request.Exclusive
}

// Lock applies a vector on a private copy, publishing it only on full success.
// Owner zero is filled from id; a different explicit owner is rejected.
// Free ranges always succeed, including requests without FAIL_IMMEDIATELY.
// Conflicts return LOCK_NOT_GRANTED immediately; unlock requires the exact owner
// and range and returns RANGE_NOT_LOCKED when there is no match.
func (table *Table) Lock(id FileID, binding Binding, ranges []Range, unlock bool) smb.Status {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return status
	}
	if len(ranges) == 0 {
		return smb.StatusInvalidParameter
	}
	record := table.objects[open.Object]
	locks := slices.Clone(record.Locks)
	for _, requested := range ranges {
		if !validRange(requested.Offset, requested.Length) {
			return smb.StatusInvalidLockRange
		}
		if requested.Owner != 0 && requested.Owner != id.Persistent {
			return smb.StatusInvalidParameter
		}
		requested.Owner = id.Persistent
		if unlock {
			index := slices.IndexFunc(locks, func(held Range) bool {
				return held.Owner == requested.Owner && held.Offset == requested.Offset && held.Length == requested.Length
			})
			if index < 0 {
				return smb.StatusRangeNotLocked
			}
			locks = slices.Delete(locks, index, index+1)
		} else {
			for _, held := range locks {
				if lockConflict(requested, held) {
					return smb.StatusLockNotGranted
				}
			}
			locks = append(locks, requested)
		}
	}
	record.Locks = locks
	return smb.StatusSuccess
}

// CheckIO follows MS-FSA 2.1.4.10. Shared locks allow reads but block writes,
// including their owner's writes. Exclusive locks allow their owner's I/O but
// block another open's reads and writes. Handlers enforce granted access.
func (table *Table) CheckIO(id FileID, binding Binding, offset, length uint64, write bool) smb.Status {
	table.mu.Lock()
	defer table.mu.Unlock()
	open, status := table.find(id, binding)
	if status != smb.StatusSuccess {
		return status
	}
	if !validRange(offset, length) {
		return smb.StatusInvalidParameter
	}
	request := Range{Owner: id.Persistent, Offset: offset, Length: length}
	for _, held := range table.objects[open.Object].Locks {
		if !overlaps(request, held) {
			continue
		}
		if (held.Exclusive && held.Owner != id.Persistent) || (!held.Exclusive && write) {
			return smb.StatusFileLockConflict
		}
	}
	return smb.StatusSuccess
}
