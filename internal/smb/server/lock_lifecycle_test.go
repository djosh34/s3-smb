package server

import (
	"fmt"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLockCloseReleasesRanges(t *testing.T) {
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	owner := insertLockOpen(t, server, session, "close-lock")
	other := insertLockOpen(t, server, session, "close-lock")
	id := session.NextMessageID
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive | lockFailImmediately}, wire.LockElement{Offset: 16, Length: 8, Flags: lockShared | lockFailImmediately}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusLockNotGranted}, lockMessage(t, session, id, other.ID, wire.LockElement{Length: 8, Flags: lockExclusive}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, closeLockOpen(t, session, id, owner.ID))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, other.ID, wire.LockElement{Length: 8, Flags: lockExclusive | lockFailImmediately}, wire.LockElement{Offset: 16, Length: 8, Flags: lockExclusive | lockFailImmediately}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusFileClosed}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockShared}))
}

func TestLockCompoundConflictDoesNotBlockDisconnect(t *testing.T) {
	for _, flags := range []uint32{lockShared, lockExclusive, lockShared | lockFailImmediately, lockExclusive | lockFailImmediately} {
		t.Run(fmt.Sprintf("flags_%x", flags), func(t *testing.T) {
			checkLockCompoundConflictDoesNotBlockDisconnect(t, flags)
		})
	}
}

func TestLockMissingRelatedFileIDAndMalformedBodyKeepConnection(t *testing.T) {
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	open := insertLockOpen(t, server, session, "invalid-body")
	id := session.NextMessageID
	prefix := sessionEcho(t, session, id)
	prefix.Header.TreeID = session.TreeID
	related := lockMessage(t, session, id+1, state.FileID{Persistent: math.MaxUint64, Volatile: math.MaxUint64}, wire.LockElement{Length: 8, Flags: lockExclusive})
	related.Header.Flags |= wire.FlagRelated
	related.Header.SessionID, related.Header.TreeID = math.MaxUint64, math.MaxUint32
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess, smb.StatusInvalidParameter}, prefix, related)
	id += 2
	malformed := lockMessage(t, session, id, open.ID, wire.LockElement{Length: 8, Flags: lockExclusive})
	malformed.Body = malformed.Body[:24]
	lockExchange(ctx, t, client, []smb.Status{smb.StatusInvalidParameter}, malformed)
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, sessionEcho(t, session, id))
	// Rejected requests must not leave an active open reference behind.
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, closeLockOpen(t, session, id, open.ID))
}
