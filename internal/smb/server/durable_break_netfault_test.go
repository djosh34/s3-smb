package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestNetworkCutCompletesOutstandingLeaseBreak(t *testing.T) {
	for _, test := range []struct {
		name    string
		access  uint32
		sharing uint32
		target  uint32
	}{
		{"remove H", fileAllAccess, 1, 0},
		{"keep H without a receiver", fileReadData, 7, smb.LeaseRead | smb.LeaseHandle},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReconnectFixture(t, smb.CipherAES128GCM)
			holder, session, transport := fixture.client(t, fixture.login.ClientGUID)
			current := uint32(smb.LeaseRead | smb.LeaseWrite | smb.LeaseHandle)
			open := fixture.durable(t, holder, &session, current, test.sharing, 0)
			requireReconnectStatus(t, fixture.io(t, holder, &session, wire.Write, open.ID, []byte("acknowledged data"), 0), smb.StatusSuccess)
			if status := fixture.lock(t, holder, &session, open.ID, 2); status != smb.StatusSuccess {
				t.Fatal(status)
			}
			peer, peerSession, _ := fixture.client(t, [16]byte{76})
			header := reconnectHeader(&peerSession, wire.Create)
			if err := peer.SendCreate(fixture.ctx, header, smbtest.CreateOptions{
				Request: wire.CreateRequest{Name: "band", DesiredAccess: test.access, ShareAccess: 7, Disposition: fileOpen},
			}); err != nil {
				t.Fatal(err)
			}
			// Receiving the notification proves CREATE is waiting on this break.
			// Deliberately do not acknowledge it before cutting the transport.
			notification, err := holder.WaitLeaseBreak(fixture.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if notification.Key != open.Lease.Key || notification.CurrentState != current || notification.NewState != test.target || notification.Flags&1 == 0 {
				t.Fatalf("wrong outstanding break: %+v", notification)
			}
			fixture.cut(t, transport)
			result, err := smbtest.DecodeCreateReply(reconnectReceive(fixture.ctx, t, peer, header))
			if err != nil {
				t.Fatal(err)
			}
			selected, err := fixture.storage.Lookup(fixture.ctx, "band")
			if err != nil {
				t.Fatal(err)
			}
			if lease, exists := fixture.server.options.State.LeaseFor(selected.Object, state.GUID(open.ClientGUID), state.GUID(open.Lease.Key)); exists {
				t.Fatalf("detached break retained lease: %+v", lease)
			}
			if status := fixture.lock(t, peer, &peerSession, result.Reply.ID, 2); status != smb.StatusSuccess {
				t.Fatalf("detached break retained range: %#x", status)
			}
			requireReconnectData(t, fixture.io(t, peer, &peerSession, wire.Read, result.Reply.ID, []byte("acknowledged data"), 0), "acknowledged data")
			fixture.refused(t, session, open)
		})
	}
}

func TestDetachedDurableOpenClosesForHandleBreak(t *testing.T) {
	fixture := newReconnectFixture(t, smb.CipherAES128GCM)
	client, session, transport := fixture.client(t, fixture.login.ClientGUID)
	open := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 1, 0)
	requireReconnectStatus(t, fixture.io(t, client, &session, wire.Write, open.ID, []byte("acknowledged data"), 0), smb.StatusSuccess)
	if status := fixture.lock(t, client, &session, open.ID, 2); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	fixture.cut(t, transport)
	peer, peerSession, _ := fixture.client(t, [16]byte{76})
	result, err := smbtest.DecodeCreateReply(fixture.create(t, peer, &peerSession, smbtest.CreateOptions{
		Request: wire.CreateRequest{Name: "band", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpen},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if status := fixture.lock(t, peer, &peerSession, result.Reply.ID, 2); status != smb.StatusSuccess {
		t.Fatalf("H-removing break retained range: %#x", status)
	}
	requireReconnectData(t, fixture.io(t, peer, &peerSession, wire.Read, result.Reply.ID, []byte("acknowledged data"), 0), "acknowledged data")
	fixture.refused(t, session, open)
}
