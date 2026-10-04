package server

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func lockServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = fuzzStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func insertLockOpen(t *testing.T, server *Server, session smbtest.Session, path string) state.Open {
	t.Helper()
	storage := server.options.Storage
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Exists {
		resolved, err = storage.Create(t.Context(), resolved.Name, smb.KindFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	reservation, status := server.options.State.Reserve(state.OpenRequest{
		Object: resolved.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID},
		User: server.options.Account.User, Share: server.options.ShareName, GrantedAccess: 3, Sharing: 7,
	})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		if abortStatus := server.options.State.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(err)
	}
	open, status := server.options.State.Commit(reservation, state.Grant{Handle: handle})
	if status != smb.StatusSuccess {
		if err := storage.Close(context.WithoutCancel(t.Context()), handle); err != nil {
			t.Error(err)
		}
		if abortStatus := server.options.State.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(status)
	}
	return open
}

func lockMessage(t *testing.T, session smbtest.Session, messageID uint64, id state.FileID, elements ...wire.LockElement) wire.Message {
	t.Helper()
	body, err := wire.EncodeLockRequest(wire.LockRequest{ID: wire.FileID(id), Elements: elements})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.Lock, MessageID: messageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 16}, Body: body}
}

func lockExchange(ctx context.Context, t *testing.T, client *smbtest.Client, want []smb.Status, messages ...wire.Message) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	responses := exchange(bounded, t, client, messages...)
	if len(responses) != len(want) {
		t.Fatalf("got %d replies, want %d", len(responses), len(want))
	}
	if len(messages) != len(want) {
		t.Fatal("reply expectations do not match requests")
	}
	for index := 0; index < len(responses) && index < len(want) && index < len(messages); index++ {
		response := responses[index]
		status := want[index]
		if response.Header.Status != status || response.Header.Flags&wire.FlagAsync != 0 || response.Header.MessageID != messages[index].Header.MessageID {
			t.Fatalf("reply %d: %+v, want status %#x and a synchronous reply", index, response.Header, status)
		}
		if status == smb.StatusSuccess && response.Header.Command == wire.Lock {
			if _, err := wire.DecodeLockResponse(response); err != nil {
				t.Fatal(err)
			}
		} else if status != smb.StatusSuccess {
			if _, err := wire.DecodeErrorResponse(response); err != nil {
				t.Fatal(err)
			}
		}
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
					if !conflict {
						lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, other.ID, wire.LockElement{Offset: 32, Length: 8, Flags: lockUnlock}, wire.LockElement{Offset: offset, Length: 8, Flags: lockUnlock}))
						id++
					} else {
						// A failed vector must not keep its otherwise-free first range.
						lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 32, Length: 8, Flags: lockExclusive}))
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
		lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidParameter}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive}, wire.LockElement{Offset: 16, Length: 8, Flags: flags}))
		id++
	}
	lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidParameter}, lockMessage(t, session, id, owner.ID, wire.LockElement{Flags: lockUnlock}, wire.LockElement{Flags: lockShared}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidLockRange}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive}, wire.LockElement{Offset: math.MaxUint64, Length: 2, Flags: lockExclusive}))
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
