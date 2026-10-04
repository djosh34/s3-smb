package server

import (
	"fmt"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLockRejectsDirectoryWithoutMutation(t *testing.T) {
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	id := session.NextMessageID
	create := wire.CreateRequest{Name: "lock-directory", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileCreateDisposition, Options: fileDirectoryFile}
	owner := createLockOpen(ctx, t, server, client, session, id, create)
	id++
	create.Disposition = fileOpen
	peer := createLockOpen(ctx, t, server, client, session, id, create)
	id++
	// Seed a range below the handler to prove rejected unlocks preserve it.
	if status := server.options.State.Lock(owner.ID, owner.Binding, []state.Range{{Length: 8, Exclusive: true}}, false); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	for _, flags := range []uint32{lockShared, lockExclusive, lockShared | lockFailImmediately, lockExclusive | lockFailImmediately, lockUnlock} {
		elements := []wire.LockElement{{Offset: 16, Length: 8, Flags: flags}, {Offset: 32, Length: 8, Flags: flags}}
		if flags == lockUnlock {
			elements = []wire.LockElement{{Length: 8, Flags: flags}}
		}
		lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidParameter}, lockMessage(t, session, id, owner.ID, elements...))
		id++
		if status := server.options.State.CheckIO(peer.ID, peer.Binding, 0, 8, true); status != smb.StatusFileLockConflict {
			t.Fatalf("rejected directory vector changed existing range: %#x", status)
		}
		for _, offset := range []uint64{16, 32} {
			if status := server.options.State.CheckIO(peer.ID, peer.Binding, offset, 8, true); status != smb.StatusSuccess {
				t.Fatalf("rejected directory vector added range at %d: %#x", offset, status)
			}
		}
	}
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, sessionEcho(t, session, id))
	id++
	if response := fileClose(ctx, t, client, session, id, wire.FileID(owner.ID), 0); response.Header.Status != smb.StatusSuccess {
		t.Fatalf("directory CLOSE: %#x", response.Header.Status)
	}
}

func TestLockVectorsNeverWait(t *testing.T) {
	for _, flags := range []uint32{lockShared, lockExclusive, lockShared | lockFailImmediately, lockExclusive | lockFailImmediately} {
		for _, compound := range []bool{false, true} {
			t.Run(fmt.Sprintf("flags_%x_compound_%t", flags, compound), func(t *testing.T) {
				server := lockServer(t)
				client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
				owner := insertLockOpen(t, server, session, "locks")
				other := insertLockOpen(t, server, session, "locks")
				id := session.NextMessageID
				lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive}))
				id++
				for _, conflict := range []bool{false, true} {
					offset := uint64(16)
					want := smb.StatusSuccess
					if conflict {
						offset, want = 0, smb.StatusLockNotGranted
					}
					if flags&lockFailImmediately == 0 {
						want = smb.StatusInvalidParameter
					}
					message := lockMessage(t, session, id, other.ID, wire.LockElement{Offset: 32, Length: 8, Flags: flags}, wire.LockElement{Offset: offset, Length: 8, Flags: flags})
					if compound {
						prefix := lockMessage(t, session, id, other.ID, wire.LockElement{Offset: 64 + offset, Length: 8, Flags: flags})
						message = lockMessage(t, session, id+1, state.FileID{Persistent: math.MaxUint64, Volatile: math.MaxUint64}, wire.LockElement{Offset: 32, Length: 8, Flags: flags}, wire.LockElement{Offset: offset, Length: 8, Flags: flags})
						message.Header.Flags |= wire.FlagRelated
						message.Header.SessionID, message.Header.TreeID = math.MaxUint64, math.MaxUint32
						lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess, want}, prefix, message)
						id += 2
					} else {
						lockExchange(ctx, t, client, []smb.Status{want}, message)
						id++
					}
					if want == smb.StatusSuccess {
						lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, other.ID, wire.LockElement{Offset: 32, Length: 8, Flags: lockUnlock}, wire.LockElement{Offset: offset, Length: 8, Flags: lockUnlock}))
						id++
					} else {
						// A failed vector must not keep its otherwise-free first range.
						lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 32, Length: 8, Flags: lockExclusive}))
						id++
						lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 32, Length: 8, Flags: lockUnlock}))
						id++
					}
				}
				lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, sessionEcho(t, session, id))
			})
		}
	}
}

func TestLockUnlockRequiresExactOwnerAndRange(t *testing.T) {
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningCMAC)
	owner := insertLockOpen(t, server, session, "unlock")
	other := insertLockOpen(t, server, session, "unlock")
	id := session.NextMessageID
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 8, Length: 8, Flags: lockExclusive}))
	for _, test := range []struct {
		id     state.FileID
		offset uint64
		length uint64
	}{{owner.ID, 8, 7}, {owner.ID, 9, 8}, {other.ID, 8, 8}} {
		id++
		lockExchange(ctx, t, client, []smb.Status{smb.StatusRangeNotLocked}, lockMessage(t, session, id, test.id, wire.LockElement{Offset: test.offset, Length: test.length, Flags: lockUnlock}))
	}
	id++
	// Failed unlock vectors also leave earlier entries untouched.
	lockExchange(ctx, t, client, []smb.Status{smb.StatusRangeNotLocked}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 8, Length: 8, Flags: lockUnlock}, wire.LockElement{Offset: 32, Length: 8, Flags: lockUnlock}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusLockNotGranted}, lockMessage(t, session, id, other.ID, wire.LockElement{Offset: 8, Length: 8, Flags: lockExclusive}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 8, Length: 8, Flags: lockUnlock}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, other.ID, wire.LockElement{Offset: 8, Length: 8, Flags: lockExclusive}))
}

func TestLockRejectsInvalidVectorsWithoutMutation(t *testing.T) {
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	owner := insertLockOpen(t, server, session, "invalid")
	other := insertLockOpen(t, server, session, "invalid")
	id := session.NextMessageID
	for _, flags := range []uint32{0, 3, 5, 6, 0x14, 0x20, math.MaxUint32} {
		lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidParameter}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive | lockFailImmediately}, wire.LockElement{Offset: 16, Length: 8, Flags: flags | lockFailImmediately}))
		id++
	}
	lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidParameter}, lockMessage(t, session, id, owner.ID, wire.LockElement{Flags: lockUnlock}, wire.LockElement{Flags: lockShared}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidLockRange}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive | lockFailImmediately}, wire.LockElement{Offset: math.MaxUint64, Length: 2, Flags: lockExclusive | lockFailImmediately}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, other.ID, wire.LockElement{Length: 8, Flags: lockExclusive}))
	id++
	stale := owner.ID
	stale.Volatile++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusFileClosed}, lockMessage(t, session, id, stale, wire.LockElement{Offset: 16, Length: 8, Flags: lockExclusive}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusFileClosed}, lockMessage(t, session, id, state.FileID{Persistent: math.MaxUint64, Volatile: math.MaxUint64}, wire.LockElement{Flags: lockShared}))
}

func TestLockZeroByteRulesAndLastByte(t *testing.T) {
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	owner := insertLockOpen(t, server, session, "zero")
	other := insertLockOpen(t, server, session, "zero")
	id := session.NextMessageID
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 8, Length: 8, Flags: lockExclusive}))
	for _, test := range []struct {
		offset uint64
		want   smb.Status
	}{{0, smb.StatusSuccess}, {8, smb.StatusSuccess}, {9, smb.StatusLockNotGranted}, {16, smb.StatusSuccess}} {
		id++
		lockExchange(ctx, t, client, []smb.Status{test.want}, lockMessage(t, session, id, other.ID, wire.LockElement{Offset: test.offset, Flags: lockExclusive}))
	}
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: math.MaxUint64, Length: 1, Flags: lockExclusive}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusLockNotGranted}, lockMessage(t, session, id, other.ID, wire.LockElement{Offset: math.MaxUint64, Length: 1, Flags: lockExclusive}))
}
