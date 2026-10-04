package server

import (
	"context"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func reconnectHeader(session *smbtest.Session, command wire.Command) wire.Header {
	header := wire.Header{Command: command, MessageID: session.NextMessageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 32}
	session.NextMessageID++
	return header
}

func reconnectReceive(ctx context.Context, t *testing.T, client *smbtest.Client, header wire.Header) wire.Message {
	t.Helper()
	for {
		reply, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Messages) != 1 {
			t.Fatal("expected one reconnect test reply")
		}
		message := reply.Messages[0]
		if message.Header.MessageID != header.MessageID || message.Header.Command != header.Command || message.Header.SessionID != header.SessionID {
			t.Fatalf("wrong reconnect reply: %+v", message.Header)
		}
		if message.Header.Status != smb.StatusPending {
			return message
		}
	}
}

func (fixture *reconnectFixture) create(t *testing.T, client *smbtest.Client, session *smbtest.Session, options smbtest.CreateOptions) wire.Message {
	t.Helper()
	header := reconnectHeader(session, wire.Create)
	if err := client.SendCreate(fixture.ctx, header, options); err != nil {
		t.Fatal(err)
	}
	return reconnectReceive(fixture.ctx, t, client, header)
}

func (fixture *reconnectFixture) durable(t *testing.T, client *smbtest.Client, session *smbtest.Session, leaseState, sharing, options uint32) smbtest.RetainedOpen {
	t.Helper()
	create := wire.CreateRequest{Name: "band", DesiredAccess: fileAllAccess, ShareAccess: sharing, Disposition: fileOpenIf, Options: options}
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{74}, State: leaseState}
	request := wire.DurableRequest{CreateGUID: [16]byte{75}, Timeout: 120000}
	message := fixture.create(t, client, session, smbtest.CreateOptions{Request: create, Lease: &lease, Durable: &request})
	result, err := smbtest.DecodeCreateReply(message)
	if err != nil {
		t.Fatal(err)
	}
	if result.Lease == nil || result.Lease.State != leaseState || result.Durable == nil || result.Durable.Timeout != request.Timeout {
		t.Fatalf("missing lease or durable grant: %+v", result)
	}
	return smbtest.RetainedOpen{Request: create, ID: result.Reply.ID, Lease: *result.Lease, CreateGUID: request.CreateGUID, ClientGUID: session.ClientGUID}
}

func (fixture *reconnectFixture) cut(t *testing.T, transport *reconnectTransport) {
	t.Helper()
	if err := transport.proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	awaitReconnectEvent(fixture.ctx, t, transport.done)
}

func (fixture *reconnectFixture) reopen(t *testing.T, previous smbtest.Session, open smbtest.RetainedOpen) (*smbtest.Client, smbtest.Session, wire.FileID) {
	t.Helper()
	transport := fixture.transport(t)
	client, session, results, err := smbtest.Reconnect(fixture.ctx, transport.conn, previous, fixture.login, []smbtest.RetainedOpen{open})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if len(results) != 1 || results[0].Reply.ID.Persistent != open.ID.Persistent || results[0].Reply.ID.Volatile == open.ID.Volatile || session.SessionID == previous.SessionID {
		t.Fatalf("reconnect did not retain persistent identity with a new binding: %+v, %+v", session, results)
	}
	return client, session, results[0].Reply.ID
}

func (fixture *reconnectFixture) refused(t *testing.T, previous smbtest.Session, open smbtest.RetainedOpen) {
	t.Helper()
	transport := fixture.transport(t)
	client, err := smbtest.NewClient(transport.conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	options := fixture.login
	options.PreviousSessionID = previous.SessionID
	session, err := client.Login(fixture.ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	message := fixture.create(t, client, &session, smbtest.CreateOptions{
		Request: open.Request, Lease: &open.Lease,
		Reconnect: &wire.DurableReconnect{ID: open.ID, CreateGUID: open.CreateGUID},
	})
	if message.Header.Status != smb.StatusObjectNameNotFound {
		t.Fatalf("DH2C refusal = %#x, want OBJECT_NAME_NOT_FOUND", message.Header.Status)
	}
}

func (fixture *reconnectFixture) lock(t *testing.T, client *smbtest.Client, session *smbtest.Session, id wire.FileID, flags uint32) smb.Status {
	t.Helper()
	body, err := wire.EncodeLockRequest(wire.LockRequest{ID: id, Elements: []wire.LockElement{{Offset: 0, Length: 17, Flags: flags}}})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: reconnectHeader(session, wire.Lock), Body: body}
	return ioRoundTrip(fixture.ctx, t, client, message).Header.Status
}

func (fixture *reconnectFixture) io(t *testing.T, client *smbtest.Client, session *smbtest.Session, command wire.Command, id wire.FileID, data []byte, offset uint64) wire.Message {
	t.Helper()
	message := reconnectIO(t, session, command, id, data, offset)
	return ioRoundTrip(fixture.ctx, t, client, message)
}

func requireReconnectStatus(t *testing.T, message wire.Message, status smb.Status) {
	t.Helper()
	if message.Header.Status != status {
		t.Fatalf("%d status = %#x, want %#x", message.Header.Command, message.Header.Status, status)
	}
}

func requireReconnectData(t *testing.T, message wire.Message, want string) {
	t.Helper()
	requireReconnectStatus(t, message, smb.StatusSuccess)
	response, err := wire.DecodeReadResponse(message)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Data) != want {
		t.Fatalf("reconnected data = %q, want %q", response.Data, want)
	}
}

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

func checkDurableReconnectIO(t *testing.T, cipher uint16, command wire.Command) {
	t.Helper()
	fixture := newReconnectFixture(t, cipher)
	client, session, transport := fixture.client(t, fixture.login.ClientGUID)
	open := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 1, 0)
	acknowledged := "acknowledged data"
	requireReconnectStatus(t, fixture.io(t, client, &session, wire.Write, open.ID, []byte(acknowledged), 0), smb.StatusSuccess)
	if status := fixture.lock(t, client, &session, open.ID, 2); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	peer, peerSession, _ := fixture.client(t, [16]byte{76})
	peerOpen, err := smbtest.DecodeCreateReply(fixture.create(t, peer, &peerSession, smbtest.CreateOptions{
		Request: wire.CreateRequest{Name: "band", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen},
	}))
	if err != nil {
		t.Fatal(err)
	}
	gate := fixture.storage.arm(command)
	message := reconnectIO(t, &session, command, open.ID, []byte(acknowledged), 0)
	if sendErr := client.Send(fixture.ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	awaitReconnectEvent(fixture.ctx, t, gate.entered)
	fixture.cut(t, transport)
	awaitReconnectEvent(fixture.ctx, t, gate.canceled)
	for {
		reply, receiveErr := client.Receive(fixture.ctx)
		if receiveErr != nil {
			break
		}
		if len(reply.Messages) != 1 || reply.Messages[0].Header.Status != smb.StatusPending {
			t.Fatalf("old request completed after cut: %+v", reply)
		}
	}
	selected, err := fixture.storage.Lookup(fixture.ctx, "band")
	if err != nil {
		t.Fatal(err)
	}
	// A raw CREATE may break H and close a fully detached open. Check sharing
	// before that exception, without starting a break or mutating storage.
	token, status := fixture.server.options.State.Reserve(state.OpenRequest{
		Object: selected.Object, Binding: state.Binding{SessionID: peerSession.SessionID, TreeID: peerSession.TreeID},
		GrantedAccess: fileWriteData, Sharing: 7,
	})
	if status == smb.StatusSuccess {
		if abortStatus := fixture.server.options.State.Abort(token); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
	}
	if status != smb.StatusSharingViolation {
		t.Fatalf("detached sharing = %#x", status)
	}
	if status := fixture.lock(t, peer, &peerSession, peerOpen.Reply.ID, 2); status != smb.StatusLockNotGranted {
		t.Fatalf("detached range = %#x", status)
	}
	fixture.clock.advance(30 * time.Second)
	resumed, resumedSession, id := fixture.reopen(t, session, open)
	requireReconnectStatus(t, fixture.io(t, resumed, &resumedSession, wire.Read, open.ID, []byte(acknowledged), 0), smb.StatusFileClosed)
	requireReconnectData(t, fixture.io(t, resumed, &resumedSession, wire.Read, id, []byte(acknowledged), 0), acknowledged)
	if status := fixture.lock(t, peer, &peerSession, peerOpen.Reply.ID, 2); status != smb.StatusLockNotGranted {
		t.Fatalf("reattached range = %#x", status)
	}
	requireReconnectStatus(t, fixture.io(t, resumed, &resumedSession, wire.Write, id, []byte(" continued"), 17), smb.StatusSuccess)
	requireReconnectStatus(t, fixture.io(t, resumed, &resumedSession, wire.Flush, id, nil, 0), smb.StatusSuccess)
	want := acknowledged + " continued"
	requireReconnectData(t, fixture.io(t, resumed, &resumedSession, wire.Read, id, []byte(want), 0), want)
	if status := fixture.lock(t, resumed, &resumedSession, id, 4); status != smb.StatusSuccess {
		t.Fatalf("retained owner's unlock = %#x", status)
	}
	if status := fixture.lock(t, peer, &peerSession, peerOpen.Reply.ID, 2); status != smb.StatusSuccess {
		t.Fatalf("peer lock after unlock = %#x", status)
	}
}

func TestDurableReconnectExpiresAfterNetworkCut(t *testing.T) {
	for _, deleteOnClose := range []bool{false, true} {
		name := "retained file"
		if deleteOnClose {
			name = "pending deletion"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newReconnectFixture(t, smb.CipherAES128GCM)
			client, session, transport := fixture.client(t, fixture.login.ClientGUID)
			options := uint32(0)
			if deleteOnClose {
				options = fileDeleteOnClose
			}
			open := fixture.durable(t, client, &session, smb.LeaseRead|smb.LeaseHandle, 7, options)
			requireReconnectStatus(t, fixture.io(t, client, &session, wire.Write, open.ID, []byte("acknowledged data"), 0), smb.StatusSuccess)
			if status := fixture.lock(t, client, &session, open.ID, 2); status != smb.StatusSuccess {
				t.Fatal(status)
			}
			fixture.cut(t, transport)
			fixture.clock.advance(121 * time.Second)
			closed, removed := fixture.storage.observeCleanup()
			fixture.server.expire(fixture.ctx)
			awaitReconnectEvent(fixture.ctx, t, closed)
			if deleteOnClose {
				awaitReconnectEvent(fixture.ctx, t, removed)
			}
			fixture.refused(t, session, open)
			selected, err := fixture.storage.Lookup(fixture.ctx, "band")
			if err != nil {
				t.Fatal(err)
			}
			if selected.Exists == deleteOnClose {
				t.Fatalf("expired file exists = %v, delete-on-close = %v", selected.Exists, deleteOnClose)
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
			if !deleteOnClose {
				requireReconnectData(t, fixture.io(t, peer, &peerSession, wire.Read, result.Reply.ID, []byte("acknowledged data"), 0), "acknowledged data")
			}
		})
	}
}
