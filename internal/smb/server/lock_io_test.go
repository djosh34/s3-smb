package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func lockIO(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, open state.Open, write bool, data string, want smb.Status) {
	t.Helper()
	if len(data) != 8 {
		t.Fatal("lock I/O tests use eight-byte ranges")
		return
	}
	command := wire.Read
	var body []byte
	var err error
	if write {
		command = wire.Write
		body, err = wire.EncodeWriteRequest(wire.WriteRequest{ID: wire.FileID(open.ID), Data: []byte(data)})
	} else {
		body, err = wire.EncodeReadRequest(wire.ReadRequest{ID: wire.FileID(open.ID), Length: 8})
	}
	if err != nil {
		t.Fatal(err)
	}
	response := ioRoundTrip(ctx, t, client, ioMessage(session, id, command, body, 1))
	if response.Header.Status != want {
		t.Fatalf("command %d object %+v: status %#x, want %#x", command, open.Object, response.Header.Status, want)
	}
	if want != smb.StatusSuccess {
		if _, err := wire.DecodeErrorResponse(response); err != nil {
			t.Fatal(err)
		}
		return
	}
	if write {
		written, err := wire.DecodeWriteResponse(response)
		if err != nil || int(written.Count) != len(data) {
			t.Fatalf("WRITE: %+v, %v", written, err)
		}
	} else {
		read, err := wire.DecodeReadResponse(response)
		if err != nil || string(read.Data) != data {
			t.Fatalf("READ: %q, %v, want %q", read.Data, err, data)
		}
	}
}

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
				owner := insertLockOpen(t, server, ownerSession, path)
				peer := insertLockOpen(t, server, peerSession, path)
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
				data := "changed!"
				if index == held {
					want = smb.StatusFileLockConflict
					data = "original"
				}
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
