package state_test

import (
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestLockIOPermissions(t *testing.T) {
	for _, test := range []struct {
		name       string
		exclusive  bool
		ownerRead  smb.Status
		ownerWrite smb.Status
		otherRead  smb.Status
		otherWrite smb.Status
	}{
		{name: "shared", ownerRead: smb.StatusSuccess, ownerWrite: smb.StatusFileLockConflict, otherRead: smb.StatusSuccess, otherWrite: smb.StatusFileLockConflict},
		{name: "exclusive", exclusive: true, ownerRead: smb.StatusSuccess, ownerWrite: smb.StatusSuccess, otherRead: smb.StatusFileLockConflict, otherWrite: smb.StatusFileLockConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			owner := commit(t, table, request(1), state.Grant{})
			other := commit(t, table, request(1), state.Grant{})
			statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 10, Length: 10, Exclusive: test.exclusive}}, false), smb.StatusSuccess)
			statusIs(t, table.CheckIO(owner.ID, binding, 12, 2, false), test.ownerRead)
			statusIs(t, table.CheckIO(owner.ID, binding, 12, 2, true), test.ownerWrite)
			statusIs(t, table.CheckIO(other.ID, binding, 12, 2, false), test.otherRead)
			statusIs(t, table.CheckIO(other.ID, binding, 12, 2, true), test.otherWrite)
			statusIs(t, table.CheckIO(other.ID, binding, 0, 10, true), smb.StatusSuccess)
			statusIs(t, table.CheckIO(other.ID, binding, 20, 1, true), smb.StatusSuccess)
		})
	}
}

func TestLockConflictsNeverWait(t *testing.T) {
	for _, test := range []struct {
		name          string
		heldExclusive bool
		newExclusive  bool
		sameOwner     bool
		want          smb.Status
	}{
		{name: "shared with shared", want: smb.StatusSuccess},
		{name: "shared blocks exclusive", newExclusive: true, want: smb.StatusLockNotGranted},
		{name: "shared blocks owner exclusive", newExclusive: true, sameOwner: true, want: smb.StatusLockNotGranted},
		{name: "exclusive blocks shared", heldExclusive: true, want: smb.StatusLockNotGranted},
		{name: "exclusive blocks exclusive", heldExclusive: true, newExclusive: true, want: smb.StatusLockNotGranted},
		{name: "exclusive blocks owner exclusive", heldExclusive: true, newExclusive: true, sameOwner: true, want: smb.StatusLockNotGranted},
		{name: "exclusive allows owner shared", heldExclusive: true, sameOwner: true, want: smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			owner := commit(t, table, request(1), state.Grant{})
			other := commit(t, table, request(1), state.Grant{})
			statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 1, Length: 10, Exclusive: test.heldExclusive}}, false), smb.StatusSuccess)
			if test.sameOwner {
				other = owner
			}
			// The table has no wait flag or wait path. This call must return.
			statusIs(t, table.Lock(other.ID, binding, []state.Range{{Offset: 2, Length: 5, Exclusive: test.newExclusive}}, false), test.want)
		})
	}
}

func TestUnlockRequiresExactOwnerAndRange(t *testing.T) {
	table := newTable(t)
	owner := commit(t, table, request(1), state.Grant{})
	other := commit(t, table, request(1), state.Grant{})
	lock := state.Range{Offset: 10, Length: 10, Exclusive: true}
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{lock}, false), smb.StatusSuccess)
	statusIs(t, table.Lock(other.ID, binding, []state.Range{lock}, true), smb.StatusRangeNotLocked)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 10, Length: 9}}, true), smb.StatusRangeNotLocked)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 11, Length: 10}}, true), smb.StatusRangeNotLocked)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Owner: other.ID.Persistent, Offset: 10, Length: 10}}, true), smb.StatusInvalidParameter)
	statusIs(t, table.CheckIO(other.ID, binding, 15, 1, false), smb.StatusFileLockConflict)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 10, Length: 10}}, true), smb.StatusSuccess)
	statusIs(t, table.CheckIO(other.ID, binding, 15, 1, false), smb.StatusSuccess)
}

func TestLockAndUnlockVectorsAreAtomic(t *testing.T) {
	table := newTable(t)
	owner := commit(t, table, request(1), state.Grant{})
	other := commit(t, table, request(1), state.Grant{})
	held := state.Range{Offset: 10, Length: 10, Exclusive: true}
	free := state.Range{Offset: 30, Length: 10, Exclusive: true}
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{held}, false), smb.StatusSuccess)
	statusIs(t, table.Lock(other.ID, binding, []state.Range{free, held}, false), smb.StatusLockNotGranted)
	statusIs(t, table.CheckIO(owner.ID, binding, 35, 1, true), smb.StatusSuccess)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{held, free}, true), smb.StatusRangeNotLocked)
	statusIs(t, table.CheckIO(other.ID, binding, 15, 1, true), smb.StatusFileLockConflict)
	statusIs(t, table.Lock(other.ID, binding, []state.Range{free, {Offset: 31, Length: 2, Exclusive: true}}, false), smb.StatusLockNotGranted)
	statusIs(t, table.CheckIO(owner.ID, binding, 35, 1, true), smb.StatusSuccess)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{free, {Offset: 50, Length: 2, Exclusive: true}}, false), smb.StatusSuccess)
	statusIs(t, table.CheckIO(other.ID, binding, 51, 1, false), smb.StatusFileLockConflict)
}

func TestRangeOverflowRollsBack(t *testing.T) {
	table := newTable(t)
	owner := commit(t, table, request(1), state.Grant{})
	other := commit(t, table, request(1), state.Grant{})
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: 10, Length: 10, Exclusive: true}, {Offset: math.MaxUint64, Length: 1}}, false), smb.StatusInvalidParameter)
	statusIs(t, table.CheckIO(other.ID, binding, 15, 1, true), smb.StatusSuccess)
	statusIs(t, table.CheckIO(owner.ID, binding, math.MaxUint64, 1, false), smb.StatusInvalidParameter)
	statusIs(t, table.Lock(owner.ID, binding, []state.Range{{Offset: math.MaxUint64 - 1, Length: 1, Exclusive: true}}, false), smb.StatusSuccess)
	statusIs(t, table.Lock(owner.ID, binding, nil, false), smb.StatusInvalidParameter)
}

func TestZeroByteLocks(t *testing.T) {
	for _, test := range []struct {
		name      string
		held      state.Range
		requested state.Range
		want      smb.Status
	}{
		{name: "origin empty never conflicts", held: state.Range{Offset: 0, Length: 10}, requested: state.Range{}, want: smb.StatusSuccess},
		{name: "origin empty held never conflicts", requested: state.Range{Offset: 0, Length: 10}, want: smb.StatusSuccess},
		{name: "empty at start", held: state.Range{Offset: 10, Length: 10}, requested: state.Range{Offset: 10}, want: smb.StatusSuccess},
		{name: "empty at end", held: state.Range{Offset: 10, Length: 10}, requested: state.Range{Offset: 20}, want: smb.StatusSuccess},
		{name: "empty inside", held: state.Range{Offset: 10, Length: 10}, requested: state.Range{Offset: 15}, want: smb.StatusLockNotGranted},
		{name: "empty held inside", held: state.Range{Offset: 15}, requested: state.Range{Offset: 10, Length: 10}, want: smb.StatusLockNotGranted},
		{name: "empty held at start", held: state.Range{Offset: 10}, requested: state.Range{Offset: 10, Length: 10}, want: smb.StatusSuccess},
		{name: "two empty locks", held: state.Range{Offset: 15}, requested: state.Range{Offset: 15}, want: smb.StatusSuccess},
		{name: "empty at maximum", held: state.Range{Offset: math.MaxUint64 - 1, Length: 1}, requested: state.Range{Offset: math.MaxUint64}, want: smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := newTable(t)
			owner := commit(t, table, request(1), state.Grant{})
			other := commit(t, table, request(1), state.Grant{})
			test.held.Exclusive, test.requested.Exclusive = true, true
			statusIs(t, table.Lock(owner.ID, binding, []state.Range{test.held}, false), smb.StatusSuccess)
			statusIs(t, table.Lock(other.ID, binding, []state.Range{test.requested}, false), test.want)
			ioStatus := smb.StatusSuccess
			if test.want != smb.StatusSuccess {
				ioStatus = smb.StatusFileLockConflict
			}
			statusIs(t, table.CheckIO(other.ID, binding, test.requested.Offset, test.requested.Length, true), ioStatus)
		})
	}
}

func TestStreamLocksAndCloseAreIsolated(t *testing.T) {
	table := newTable(t)
	owner := commit(t, table, requestWithStream(1, "one"), state.Grant{})
	same := commit(t, table, requestWithStream(1, "one"), state.Grant{})
	other := commit(t, table, requestWithStream(1, "two"), state.Grant{})
	base := commit(t, table, request(1), state.Grant{})
	lock := []state.Range{{Length: 10, Exclusive: true}}
	statusIs(t, table.Lock(owner.ID, binding, lock, false), smb.StatusSuccess)
	statusIs(t, table.CheckIO(same.ID, binding, 0, 10, true), smb.StatusFileLockConflict)
	statusIs(t, table.CheckIO(other.ID, binding, 0, 10, true), smb.StatusSuccess)
	statusIs(t, table.CheckIO(base.ID, binding, 0, 10, true), smb.StatusSuccess)
	statusIs(t, table.Lock(other.ID, binding, lock, false), smb.StatusSuccess)
	closeOpen(t, table, owner)
	statusIs(t, table.CheckIO(same.ID, binding, 0, 10, true), smb.StatusSuccess)
}

func TestLockDoesNotAliasCallerVector(t *testing.T) {
	table := newTable(t)
	owner := commit(t, table, request(1), state.Grant{})
	other := commit(t, table, request(1), state.Grant{})
	vector := []state.Range{{Length: 10, Exclusive: true}}
	statusIs(t, table.Lock(owner.ID, binding, vector, false), smb.StatusSuccess)
	vector[0].Offset = 100
	statusIs(t, table.CheckIO(other.ID, binding, 1, 1, false), smb.StatusFileLockConflict)
}
