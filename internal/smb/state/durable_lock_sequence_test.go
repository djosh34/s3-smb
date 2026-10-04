package state_test

import (
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestDurableLockSequenceIndices(t *testing.T) {
	for _, index := range []uint32{0, 1, 64, 65, 0x0fffffff} {
		for _, number := range []uint32{0, 15} {
			t.Run(fmt.Sprintf("index%d/number%d", index, number), func(t *testing.T) {
				table := newTable(t)
				req := durableRequest(1, 2)
				open := commit(t, table, req, durableGrant(req))
				ranges := []state.Range{{Length: 10, Exclusive: true}}
				sequence := index<<4 | number
				statusIs(t, table.LockSequence(open.ID, binding, ranges, false, sequence), smb.StatusSuccess)
				want := smb.StatusSuccess
				if index == 0 || index > 64 {
					want = smb.StatusLockNotGranted
				}
				statusIs(t, table.LockSequence(open.ID, binding, ranges, false, sequence), want)
				statusIs(t, table.Lock(open.ID, binding, ranges, true), smb.StatusSuccess)
				statusIs(t, table.Lock(open.ID, binding, ranges, true), smb.StatusRangeNotLocked)
			})
		}
	}
}

func TestDurableLockSequenceNumbersAndUnlockReplay(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	peer := commit(t, table, request(1), state.Grant{})
	ranges := []state.Range{{Length: 10, Exclusive: true}}
	// Exercise all four-bit values, including the 15 -> 0 transition.
	for number := uint32(0); number < 32; number += 2 {
		lockSequence := uint32(16) | number&15
		unlockSequence := uint32(16) | (number+1)&15
		statusIs(t, table.LockSequence(open.ID, binding, ranges, false, lockSequence), smb.StatusSuccess)
		statusIs(t, table.LockSequence(open.ID, binding, ranges, false, lockSequence), smb.StatusSuccess)
		statusIs(t, table.CheckIO(peer.ID, binding, 1, 1, false), smb.StatusFileLockConflict)
		statusIs(t, table.LockSequence(open.ID, binding, ranges, true, unlockSequence), smb.StatusSuccess)
		statusIs(t, table.LockSequence(open.ID, binding, ranges, true, unlockSequence), smb.StatusSuccess)
		statusIs(t, table.CheckIO(peer.ID, binding, 1, 1, true), smb.StatusSuccess)
	}
	// Replay is keyed by number, not a fingerprint of the range vector.
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{{Offset: 30, Length: 10, Exclusive: true}}, false, 31), smb.StatusSuccess)
	statusIs(t, table.CheckIO(peer.ID, binding, 31, 1, true), smb.StatusSuccess)
}

func TestDurableLockSequenceFailureInvalidatesOnlyHistory(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	peer := commit(t, table, request(1), state.Grant{})
	held := state.Range{Length: 10, Exclusive: true}
	free := state.Range{Offset: 30, Length: 10, Exclusive: true}
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{held}, false, 16), smb.StatusSuccess)
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{free, held}, false, 17), smb.StatusLockNotGranted)
	statusIs(t, table.CheckIO(peer.ID, binding, 31, 1, true), smb.StatusSuccess)
	statusIs(t, table.CheckIO(peer.ID, binding, 1, 1, true), smb.StatusFileLockConflict)
	// Neither the old number nor the failed replacement may replay success.
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{held}, false, 16), smb.StatusLockNotGranted)
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{held}, false, 17), smb.StatusLockNotGranted)
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{held, free}, true, 18), smb.StatusRangeNotLocked)
	statusIs(t, table.CheckIO(peer.ID, binding, 1, 1, true), smb.StatusFileLockConflict)
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{free, {Offset: math.MaxUint64, Length: 2}}, false, 19), smb.StatusInvalidLockRange)
	statusIs(t, table.CheckIO(peer.ID, binding, 31, 1, true), smb.StatusSuccess)
	statusIs(t, table.LockSequence(open.ID, binding, []state.Range{free}, false, 19), smb.StatusSuccess)
	statusIs(t, table.CheckIO(peer.ID, binding, 31, 1, true), smb.StatusFileLockConflict)
}

func TestDurableLockSequenceIsolatedByOpenAndSlot(t *testing.T) {
	table := newTable(t)
	firstReq, secondReq := durableRequest(1, 2), durableRequest(1, 3)
	first := commit(t, table, firstReq, durableGrant(firstReq))
	secondGrant := durableGrant(secondReq)
	secondGrant.Lease.Key = firstReq.CreateGUID
	second := commit(t, table, secondReq, secondGrant)
	ranges := []state.Range{{Length: 10, Exclusive: true}}
	statusIs(t, table.LockSequence(first.ID, binding, ranges, false, 16), smb.StatusSuccess)
	statusIs(t, table.LockSequence(second.ID, binding, ranges, false, 16), smb.StatusLockNotGranted)
	statusIs(t, table.LockSequence(first.ID, binding, ranges, false, 32), smb.StatusLockNotGranted)
	statusIs(t, table.LockSequence(first.ID, binding, ranges, false, 16), smb.StatusSuccess)
	statusIs(t, table.Lock(first.ID, binding, ranges, true), smb.StatusSuccess)
	statusIs(t, table.LockSequence(second.ID, binding, ranges, false, 16), smb.StatusSuccess)
}

func TestDurableLockSequenceSurvivesReconnect(t *testing.T) {
	table, _ := clockTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	ranges := []state.Range{{Length: 10, Exclusive: true}}
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusSuccess)
	if actions := table.Disconnect(binding.SessionID); len(actions) != 0 {
		t.Fatal(actions)
	}
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 17), smb.StatusFileClosed)
	reattached, status := table.Reconnect(reconnectRequest(open))
	statusIs(t, status, smb.StatusSuccess)
	statusIs(t, table.LockSequence(open.ID, reattached.Binding, ranges, false, 17), smb.StatusFileClosed)
	statusIs(t, table.LockSequence(reattached.ID, reattached.Binding, ranges, false, 16), smb.StatusSuccess)
	statusIs(t, table.Lock(reattached.ID, reattached.Binding, ranges, true), smb.StatusSuccess)
	statusIs(t, table.Lock(reattached.ID, reattached.Binding, ranges, true), smb.StatusRangeNotLocked)
}

func TestDurableLockSequenceClosesWithOpen(t *testing.T) {
	for _, expire := range []bool{false, true} {
		name := "close"
		if expire {
			name = "expiry"
		}
		t.Run(name, func(t *testing.T) {
			table, now := clockTable(t)
			req := durableRequest(1, 2)
			open := commit(t, table, req, durableGrant(req))
			ranges := []state.Range{{Length: 10, Exclusive: true}}
			statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusSuccess)
			if expire {
				if actions := table.Disconnect(binding.SessionID); len(actions) != 0 {
					t.Fatal(actions)
				}
				*now = now.Add(smb.DefaultDurableTimeout)
				if actions := table.Expire(); len(actions) != 1 || actions[0].Handle != open.Handle {
					t.Fatal(actions)
				}
			} else {
				closeOpen(t, table, open)
			}
			statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusFileClosed)
			reopened := commit(t, table, req, durableGrant(req))
			peer := commit(t, table, request(1), state.Grant{})
			statusIs(t, table.LockSequence(reopened.ID, binding, ranges, false, 16), smb.StatusSuccess)
			statusIs(t, table.CheckIO(peer.ID, binding, 1, 1, false), smb.StatusFileLockConflict)
		})
	}
}

func TestOrdinaryOpenIgnoresLockSequence(t *testing.T) {
	table := newTable(t)
	open := commit(t, table, request(1), state.Grant{})
	ranges := []state.Range{{Length: 10, Exclusive: true}}
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusSuccess)
	statusIs(t, table.LockSequence(open.ID, binding, ranges, false, 16), smb.StatusLockNotGranted)
	statusIs(t, table.LockSequence(open.ID, binding, ranges, true, 17), smb.StatusSuccess)
	statusIs(t, table.LockSequence(open.ID, binding, ranges, true, 17), smb.StatusRangeNotLocked)
}

func TestDurableLockSequenceConcurrentReplay(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	ranges := []state.Range{{Length: 10, Exclusive: true}}
	results := make(chan smb.Status, 32)
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() { results <- table.LockSequence(open.ID, binding, ranges, false, 16) })
	}
	workers.Wait()
	close(results)
	for result := range results {
		statusIs(t, result, smb.StatusSuccess)
	}
	statusIs(t, table.Lock(open.ID, binding, ranges, true), smb.StatusSuccess)
	statusIs(t, table.Lock(open.ID, binding, ranges, true), smb.StatusRangeNotLocked)
}
