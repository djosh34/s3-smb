package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLockMultiRangeRequiresFailImmediately(t *testing.T) {
	flags := []uint32{lockShared, lockExclusive, lockShared | lockFailImmediately, lockExclusive | lockFailImmediately}
	for _, first := range flags {
		for _, second := range flags {
			t.Run(fmt.Sprintf("flags_%x_%x", first, second), func(t *testing.T) {
				server := lockServer(t)
				client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
				owner := insertLockOpen(t, server, session, "lock-vector-flags")
				peer := insertLockOpen(t, server, session, "lock-vector-flags")
				id := session.NextMessageID
				lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, owner.ID, wire.LockElement{Length: 8, Flags: lockExclusive}))
				id++

				want, wantIO := smb.StatusInvalidParameter, smb.StatusSuccess
				if first&lockFailImmediately != 0 && second&lockFailImmediately != 0 {
					want, wantIO = smb.StatusSuccess, smb.StatusFileLockConflict
				}
				lockExchange(ctx, t, client, []smb.Status{want}, lockMessage(t, session, id, owner.ID, wire.LockElement{Offset: 16, Length: 8, Flags: first}, wire.LockElement{Offset: 32, Length: 8, Flags: second}))
				id++
				if status := server.options.State.CheckIO(peer.ID, peer.Binding, 0, 8, true); status != smb.StatusFileLockConflict {
					t.Fatalf("vector changed existing lock: %#x", status)
				}
				for _, offset := range []uint64{16, 32} {
					if status := server.options.State.CheckIO(peer.ID, peer.Binding, offset, 8, true); status != wantIO {
						t.Fatalf("vector lock at %d: %#x, want %#x", offset, status, wantIO)
					}
				}
				lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, sessionEcho(t, session, id))
				id++
				lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, closeLockOpen(t, session, id, owner.ID))
				id++
				lockExchange(ctx, t, client, []smb.Status{smb.StatusSuccess}, lockMessage(t, session, id, peer.ID, wire.LockElement{Length: 8, Flags: lockExclusive | lockFailImmediately}, wire.LockElement{Offset: 16, Length: 8, Flags: lockExclusive | lockFailImmediately}, wire.LockElement{Offset: 32, Length: 8, Flags: lockExclusive | lockFailImmediately}))
			})
		}
	}
}
