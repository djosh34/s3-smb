package server

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestDurableReconnectDuringIO(t *testing.T) {
	for _, protection := range []struct {
		name   string
		cipher uint16
	}{{"signed", 0}, {"encrypted", smb.CipherAES256GCM}} {
		for _, operation := range []struct {
			name    string
			command wire.Command
		}{{"write", wire.Write}, {"read", wire.Read}, {"flush", wire.Flush}} {
			t.Run(protection.name+"/"+operation.name, func(t *testing.T) {
				checkDurableReconnectIO(t, protection.cipher, operation.command)
			})
		}
	}
}

func TestDurableReconnectExpiresAfterNetworkCut(t *testing.T) {
	for _, test := range []struct {
		name            string
		deleteOnClose   bool
		expireOnAdvance bool
	}{
		{name: "retained file"},
		{name: "pending deletion", deleteOnClose: true},
		{name: "cleanup during advance/retained file", expireOnAdvance: true},
		{name: "cleanup during advance/pending deletion", deleteOnClose: true, expireOnAdvance: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReconnectFixture(t, smb.CipherAES128GCM)
			if test.expireOnAdvance {
				// Force the live scavenger's worst-case ordering: cleanup finishes
				// before advance returns, rather than after the explicit expiry pass.
				fixture.clock.afterAdvance = func() {
					fixture.server.expire(fixture.ctx)
					fixture.server.scavengerCleanup.Wait()
				}
			}
			client, session, transport := fixture.client(t, fixture.login.ClientGUID)
			options := uint32(0)
			if test.deleteOnClose {
				options = fileDeleteOnClose
			}
			open := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 7, options)
			requireReconnectStatus(t, fixture.io(t, client, &session, wire.Write, open.ID, []byte("acknowledged data"), 0), smb.StatusSuccess)
			if status := fixture.lock(t, client, &session, open.ID, 2); status != smb.StatusSuccess {
				t.Fatal(status)
			}
			fixture.cut(t, transport)
			closed, removed := fixture.storage.observeCleanup()
			fixture.clock.advance(121 * time.Second)
			fixture.server.expire(fixture.ctx)
			awaitReconnectEvent(fixture.ctx, t, closed)
			if test.deleteOnClose {
				awaitReconnectEvent(fixture.ctx, t, removed)
			}
			fixture.refused(t, session, open)
			selected, err := fixture.storage.Lookup(fixture.ctx, "band")
			if err != nil {
				t.Fatal(err)
			}
			if selected.Exists == test.deleteOnClose {
				t.Fatalf("expired file exists = %v, delete-on-close = %v", selected.Exists, test.deleteOnClose)
			}
			peer, peerSession, _ := fixture.client(t, [16]byte{76})
			result, err := smbtest.DecodeCreateReply(fixture.create(t, peer, &peerSession, smbtest.CreateOptions{
				Request: wire.CreateRequest{Name: "band", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if status := fixture.lock(t, peer, &peerSession, result.Reply.ID, 2); status != smb.StatusSuccess {
				t.Fatalf("expired ranges survived: %#x", status)
			}
			if !test.deleteOnClose {
				requireReconnectData(t, fixture.io(t, peer, &peerSession, wire.Read, result.Reply.ID, []byte("acknowledged data"), 0), "acknowledged data")
			}
		})
	}
}
