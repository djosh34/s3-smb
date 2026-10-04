package server

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func (fixture *reconnectFixture) lockSequence(t *testing.T, client *smbtest.Client, session *smbtest.Session, id wire.FileID, sequence uint32, elements ...wire.LockElement) smb.Status {
	t.Helper()
	body, err := wire.EncodeLockRequest(wire.LockRequest{ID: id, Sequence: sequence, Elements: elements})
	if err != nil {
		t.Fatal(err)
	}
	header := reconnectHeader(session, wire.Lock)
	if sendErr := client.Send(fixture.ctx, []wire.Message{{Header: header, Body: body}}); sendErr != nil {
		t.Fatal(sendErr)
	}
	response, err := client.Receive(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatal("expected one LOCK reply")
	}
	message := response.Messages[0]
	if message.Header.MessageID != header.MessageID || message.Header.Command != wire.Lock || message.Header.Status == smb.StatusPending || message.Header.Flags&wire.FlagAsync != 0 {
		t.Fatalf("wrong or asynchronous LOCK reply: %+v", message.Header)
	}
	if message.Header.Status == smb.StatusSuccess {
		if _, decodeErr := wire.DecodeLockResponse(message); decodeErr != nil {
			t.Fatal(decodeErr)
		}
	}
	return message.Header.Status
}

func requireLockSequenceStatus(t *testing.T, got, want smb.Status) {
	t.Helper()
	if got != want {
		t.Fatalf("LOCK status = %#x, want %#x", got, want)
	}
}

func TestDurableLockSequenceRawReconnect(t *testing.T) {
	for _, protection := range []struct {
		name   string
		cipher uint16
	}{{"signed", 0}, {"encrypted", smb.CipherAES256GCM}} {
		t.Run(protection.name, func(t *testing.T) {
			fixture := newReconnectFixture(t, protection.cipher)
			client, session, transport := fixture.client(t, fixture.login.ClientGUID)
			open := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 7, 0)
			held := wire.LockElement{Length: 10, Flags: lockExclusive | lockFailImmediately}
			requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, open.ID, 16, held), smb.StatusSuccess)
			peer, peerSession, _ := fixture.client(t, [16]byte{76})
			peerOpen := durableResult(t, fixture.create(t, peer, &peerSession, smbtest.CreateOptions{
				Request: wire.CreateRequest{Name: "band", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen},
			}))
			fixture.cut(t, transport)
			requireLockSequenceStatus(t, fixture.lockSequence(t, peer, &peerSession, peerOpen.Reply.ID, 16, held), smb.StatusLockNotGranted)
			resumed, resumedSession, id := fixture.reopen(t, session, open)
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, open.ID, 17, held), smb.StatusFileClosed)
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, id, 16, held), smb.StatusSuccess)
			invalid := held
			invalid.Flags = lockShared | lockExclusive
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, id, 16, invalid), smb.StatusInvalidParameter)
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, id, 16, held), smb.StatusSuccess)
			// A failed replacement invalidates replay history, but not ranges.
			free := wire.LockElement{Offset: 30, Length: 10, Flags: held.Flags}
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, id, 17, free, held), smb.StatusLockNotGranted)
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, id, 16, held), smb.StatusLockNotGranted)
			requireLockSequenceStatus(t, fixture.lockSequence(t, peer, &peerSession, peerOpen.Reply.ID, 0, held), smb.StatusLockNotGranted)
			requireLockSequenceStatus(t, fixture.lockSequence(t, peer, &peerSession, peerOpen.Reply.ID, 0, free), smb.StatusSuccess)
			free.Flags = lockUnlock
			requireLockSequenceStatus(t, fixture.lockSequence(t, peer, &peerSession, peerOpen.Reply.ID, 0, free), smb.StatusSuccess)
			held.Flags = lockUnlock
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, id, 18, held), smb.StatusSuccess)
			requireLockSequenceStatus(t, fixture.lockSequence(t, resumed, &resumedSession, id, 18, held), smb.StatusSuccess)
			// A single unlock released the range: replay never duplicated it.
			held.Flags = lockExclusive
			requireLockSequenceStatus(t, fixture.lockSequence(t, peer, &peerSession, peerOpen.Reply.ID, 0, held), smb.StatusSuccess)
		})
	}
}

func TestDurableLockSequenceRawOpenIsolation(t *testing.T) {
	fixture := newReconnectFixture(t, smb.CipherAES128GCM)
	client, session, _ := fixture.client(t, fixture.login.ClientGUID)
	first := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 7, 0)
	second := durableResult(t, fixture.create(t, client, &session, smbtest.CreateOptions{
		Request: first.Request, Lease: &first.Lease,
		Durable: &wire.DurableRequest{CreateGUID: [16]byte{77}},
	}))
	if second.Durable == nil || second.Reply.ID.Persistent == first.ID.Persistent {
		t.Fatal("second CREATE did not grant a distinct durable open")
	}
	held := wire.LockElement{Length: 10, Flags: lockExclusive}
	requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, first.ID, 16, held), smb.StatusSuccess)
	requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, second.Reply.ID, 16, held), smb.StatusLockNotGranted)
	held.Offset = 30
	requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, second.Reply.ID, 16, held), smb.StatusSuccess)
}

func TestDurableLockSequenceRawLifetime(t *testing.T) {
	for _, expire := range []bool{false, true} {
		name := "close"
		if expire {
			name = "expiry"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newReconnectFixture(t, smb.CipherAES128GCM)
			client, session, transport := fixture.client(t, fixture.login.ClientGUID)
			open := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 7, 0)
			held := wire.LockElement{Length: 10, Flags: lockExclusive}
			requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, open.ID, 16, held), smb.StatusSuccess)
			if expire {
				fixture.cut(t, transport)
				fixture.clock.advance(121 * time.Second)
				closed, _ := fixture.storage.observeCleanup()
				fixture.server.expire(fixture.ctx)
				awaitReconnectEvent(fixture.ctx, t, closed)
				fixture.refused(t, session, open)
				client, session, _ = fixture.client(t, fixture.login.ClientGUID)
			} else {
				body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: open.ID})
				if err != nil {
					t.Fatal(err)
				}
				message := wire.Message{Header: reconnectHeader(&session, wire.Close), Body: body}
				requireReconnectStatus(t, ioRoundTrip(fixture.ctx, t, client, message), smb.StatusSuccess)
			}
			requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, open.ID, 16, held), smb.StatusFileClosed)
			replacement := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 7, 0)
			if replacement.ID.Persistent == open.ID.Persistent {
				t.Fatal("replacement reused closed open identity")
			}
			requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, replacement.ID, 16, held), smb.StatusSuccess)
			// Bypassing replay proves the new request actually reserved its range.
			requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, replacement.ID, 0, held), smb.StatusLockNotGranted)
		})
	}
}

func TestOrdinaryLockSequenceRaw(t *testing.T) {
	fixture := newReconnectFixture(t, smb.CipherAES128GCM)
	client, session, _ := fixture.client(t, fixture.login.ClientGUID)
	open := durableResult(t, fixture.create(t, client, &session, smbtest.CreateOptions{
		Request: wire.CreateRequest{Name: "ordinary", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf},
	}))
	if open.Durable != nil {
		t.Fatal("ordinary open received a durable grant")
	}
	held := wire.LockElement{Length: 10, Flags: lockExclusive}
	requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, open.Reply.ID, 16, held), smb.StatusSuccess)
	requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, open.Reply.ID, 16, held), smb.StatusLockNotGranted)
	held.Flags = lockUnlock
	requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, open.Reply.ID, 17, held), smb.StatusSuccess)
	requireLockSequenceStatus(t, fixture.lockSequence(t, client, &session, open.Reply.ID, 17, held), smb.StatusRangeNotLocked)
}
