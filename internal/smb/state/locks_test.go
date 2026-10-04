package state_test

import (
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestLockConflicts(t *testing.T) {
	for _, test := range []struct {
		name                        string
		heldExclusive, newExclusive bool
		sameOwner                   bool
		want, ownWrite, otherIO     smb.Status
	}{
		{"shared with shared", false, false, false, smb.StatusSuccess, smb.StatusFileLockConflict, smb.StatusSuccess},
		{"shared blocks exclusive", false, true, false, smb.StatusLockNotGranted, smb.StatusFileLockConflict, smb.StatusSuccess},
		{"shared blocks owner exclusive", false, true, true, smb.StatusLockNotGranted, smb.StatusFileLockConflict, smb.StatusSuccess},
		{"exclusive blocks shared", true, false, false, smb.StatusLockNotGranted, smb.StatusSuccess, smb.StatusFileLockConflict},
		{"exclusive blocks owner exclusive", true, true, true, smb.StatusLockNotGranted, smb.StatusSuccess, smb.StatusFileLockConflict},
		{"exclusive allows owner shared", true, false, true, smb.StatusSuccess, smb.StatusSuccess, smb.StatusFileLockConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			owner := commit(t, table, request(1), state.Grant{})
			other := commit(t, table, request(1), state.Grant{})
			statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 10, Length: 10, Exclusive: test.heldExclusive}}, false), smb.StatusSuccess)
			statusIs(t, table.CheckIO(owner.ID, binding, 12, 2, false), smb.StatusSuccess)
			statusIs(t, table.CheckIO(owner.ID, binding, 12, 2, true), test.ownWrite)
			statusIs(t, table.CheckIO(other.ID, binding, 12, 2, false), test.otherIO)
			statusIs(t, table.CheckIO(other.ID, binding, 0, 10, true), smb.StatusSuccess)
			requester := other
			if test.sameOwner {
				requester = owner
			}
			statusIs(t, table.Lock(requester.ID, binding, []state.Range{{Offset: 15, Length: 10, Exclusive: test.newExclusive}}, false), test.want)
		})
	}
}

func TestUnlockRequiresExactOwnerAndRange(t *testing.T) {
	table := newTable(t)
	owner := commit(t, table, request(1), state.Grant{})
	other := commit(t, table, request(1), state.Grant{})
	lock := state.Range{Offset: 10, Length: 10, Exclusive: true}
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{lock}, false), smb.StatusSuccess)
	for _, test := range []struct {
		id     state.FileID
		unlock state.Range
	}{{other.ID, lock}, {owner.ID, state.Range{Offset: 10, Length: 9}}, {owner.ID, state.Range{Offset: 11, Length: 10}}} {
		statusIs(t, table.Lock(test.id, binding, []state.Range{test.unlock}, true), smb.StatusRangeNotLocked)
	}
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 10, Length: 10}}, true), smb.StatusSuccess)
	statusIs(t, table.CheckIO(other.ID, binding, 15, 1, false), smb.StatusSuccess)
}

// A vector is applied whole or not at all.
func TestLockVectorsAreAtomic(t *testing.T) {
	table := newTable(t)
	owner := commit(t, table, request(1), state.Grant{})
	other := commit(t, table, request(1), state.Grant{})
	held := state.Range{Offset: 10, Length: 10, Exclusive: true}
	free := state.Range{Offset: 30, Length: 10, Exclusive: true}
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{held}, false), smb.StatusSuccess)
	statusIs(t, table.Lock(other.ID, binding, []state.Range{free, held}, false), smb.StatusLockNotGranted)
	statusIs(t, table.Lock(other.ID, binding, []state.Range{free, {Offset: math.MaxUint64, Length: 2}}, false), smb.StatusInvalidLockRange)
	statusIs(t, table.Lock(other.ID, binding, []state.Range{free, {Offset: 31, Length: 2, Exclusive: true}}, false), smb.StatusLockNotGranted)
	statusIs(t, table.CheckIO(owner.ID, binding, 35, 1, true), smb.StatusSuccess)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{held, free}, true), smb.StatusRangeNotLocked)
	statusIs(t, table.CheckIO(other.ID, binding, 15, 1, true), smb.StatusFileLockConflict)
	statusIs(t, table.Lock(owner.ID, binding, nil, false), smb.StatusInvalidParameter)
}

func TestLockLastByte(t *testing.T) {
	for _, held := range []state.Range{{Offset: math.MaxUint64, Length: 1, Exclusive: true}, {Offset: 1, Length: math.MaxUint64, Exclusive: true}} {
		table := newTable(t)
		owner := commit(t, table, request(1), state.Grant{})
		other := commit(t, table, request(1), state.Grant{})
		statusIs(t, table.Lock(owner.ID, binding, []state.Range{held}, false), smb.StatusSuccess)
		statusIs(t, table.CheckIO(other.ID, binding, math.MaxUint64, 1, false), smb.StatusFileLockConflict)
		statusIs(t, table.CheckIO(owner.ID, binding, math.MaxUint64, 2, false), smb.StatusInvalidParameter)
		statusIs(t, table.Lock(other.ID, binding, []state.Range{{Offset: math.MaxUint64, Length: 1}}, false), smb.StatusLockNotGranted)
		statusIs(t, table.Lock(owner.ID, binding, []state.Range{held}, true), smb.StatusSuccess)
		statusIs(t, table.CheckIO(other.ID, binding, math.MaxUint64, 1, false), smb.StatusSuccess)
	}
}

func TestZeroByteLocks(t *testing.T) {
	for _, test := range []struct {
		name            string
		held, requested state.Range
		want            smb.Status
	}{
		{"empty at origin never conflicts", state.Range{Length: 10}, state.Range{}, smb.StatusSuccess},
		{"empty at start", state.Range{Offset: 10, Length: 10}, state.Range{Offset: 10}, smb.StatusSuccess},
		{"empty at end", state.Range{Offset: 10, Length: 10}, state.Range{Offset: 20}, smb.StatusSuccess},
		{"empty inside", state.Range{Offset: 10, Length: 10}, state.Range{Offset: 15}, smb.StatusLockNotGranted},
		{"held empty inside", state.Range{Offset: 15}, state.Range{Offset: 10, Length: 10}, smb.StatusLockNotGranted},
		{"two empty locks", state.Range{Offset: 15}, state.Range{Offset: 15}, smb.StatusSuccess},
		{"empty inside the last byte", state.Range{Offset: math.MaxUint64 - 1, Length: 2}, state.Range{Offset: math.MaxUint64}, smb.StatusLockNotGranted},
		{"empty at the last byte", state.Range{Offset: math.MaxUint64, Length: 1}, state.Range{Offset: math.MaxUint64}, smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			owner := commit(t, table, request(1), state.Grant{})
			other := commit(t, table, request(1), state.Grant{})
			test.held.Exclusive, test.requested.Exclusive = true, true
			statusIs(t, table.Lock(owner.ID, binding, []state.Range{test.held}, false), smb.StatusSuccess)
			statusIs(t, table.Lock(other.ID, binding, []state.Range{test.requested}, false), test.want)
		})
	}
}
