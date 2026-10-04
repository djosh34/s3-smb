package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLockStreamAndBaseIOStaySeparate(t *testing.T) {
	for _, heldStream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", heldStream), func(t *testing.T) {
			server := lockServer(t)
			ownerClient, ownerCtx, ownerSession := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
			peerClient, peerCtx, peerSession := loginClient(t, server, smb.CipherAES256GCM, smb.SigningCMAC)
			paths := []string{"stream-lock", "stream-lock:AFP_Resource:$DATA", "stream-lock:com.apple.test:$DATA"}
			owners := make([]state.Open, 0, len(paths))
			peers := make([]state.Open, 0, len(paths))
			ownerID, peerID := ownerSession.NextMessageID, peerSession.NextMessageID
			for _, path := range paths {
				create := wire.CreateRequest{Name: path, DesiredAccess: fileReadData | fileWriteData, ShareAccess: 7, Disposition: fileOpenIf}
				owner := createLockOpen(ownerCtx, t, server, ownerClient, ownerSession, ownerID, create)
				ownerID++
				create.Disposition = fileOpen
				peer := createLockOpen(peerCtx, t, server, peerClient, peerSession, peerID, create)
				peerID++
				owners, peers = append(owners, owner), append(peers, peer)
				lockIO(ownerCtx, t, ownerClient, ownerSession, ownerID, owner, true, "original", smb.StatusSuccess)
				ownerID++
			}
			if owners[0].Object.Inode != owners[1].Object.Inode || owners[0].Object.Inode != owners[2].Object.Inode || owners[0].Object.Stream != "" || owners[1].Object.Stream == "" || owners[1].Object.Stream == owners[2].Object.Stream {
				t.Fatal("fixture did not open three separate streams of the same base file")
			}
			held := 0
			if heldStream {
				held = 1
			}
			lockExchange(ownerCtx, t, ownerClient, []smb.Status{smb.StatusSuccess}, lockMessage(t, ownerSession, ownerID, owners[held].ID, wire.LockElement{Length: 8, Flags: lockExclusive}))
			ownerID++
			for index, peer := range peers {
				want := smb.StatusSuccess
				wantLock := smb.StatusSuccess
				data := "changed!"
				if index == held {
					want = smb.StatusFileLockConflict
					wantLock = smb.StatusLockNotGranted
					data = "original"
				}
				lockExchange(peerCtx, t, peerClient, []smb.Status{wantLock}, lockMessage(t, peerSession, peerID, peer.ID, wire.LockElement{Length: 8, Flags: lockExclusive}))
				peerID++
				lockIO(peerCtx, t, peerClient, peerSession, peerID, peer, true, "changed!", want)
				peerID++
				lockIO(peerCtx, t, peerClient, peerSession, peerID, peer, false, data, want)
				peerID++
			}
			// The owner's exclusive lock allows its I/O and a rejected peer WRITE
			// must not have changed the selected stream's bytes.
			lockIO(ownerCtx, t, ownerClient, ownerSession, ownerID, owners[held], false, "original", smb.StatusSuccess)
			ownerID++
			lockIO(ownerCtx, t, ownerClient, ownerSession, ownerID, owners[held], true, "owner123", smb.StatusSuccess)
			ownerID++
			lockExchange(ownerCtx, t, ownerClient, []smb.Status{smb.StatusSuccess}, lockMessage(t, ownerSession, ownerID, owners[held].ID, wire.LockElement{Length: 8, Flags: lockUnlock}))
			lockIO(peerCtx, t, peerClient, peerSession, peerID, peers[held], false, "owner123", smb.StatusSuccess)
		})
	}
}

func TestLockSharedAllowsReadsButRejectsAllWrites(t *testing.T) {
	server := lockServer(t)
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	owner := insertLockOpen(t, server, session, "shared-lock")
	other := insertLockOpen(t, server, session, "shared-lock")
	id := session.NextMessageID
	lockIO(ctx, t, client, session, id, owner, true, "original", smb.StatusSuccess)
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockShared}))
	id++
	lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, other.ID, wire.LockElement{Length: 8, Flags: lockShared}))
	id++
	for _, open := range []state.Open{owner, other} {
		lockIO(ctx, t, client, session, id, open, false, "original", smb.StatusSuccess)
		id++
		lockIO(ctx, t, client, session, id, open, true, "changed!", smb.StatusFileLockConflict)
		id++
	}
}
