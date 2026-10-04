package server

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func leaseServer(t *testing.T, cipher, signing uint16) (*Server, *smbtest.Client, context.Context, smbtest.Session, *cleanupStorage) {
	t.Helper()
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, cipher, signing)
	return server, client, ctx, session, storage
}

func insertLeaseOpen(t *testing.T, server *Server, session smbtest.Session, inode smb.Inode, rights uint32, durable bool) state.Open {
	t.Helper()
	request := state.OpenRequest{Object: smb.ObjectKey{Inode: inode}, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: state.GUID{2}, Sharing: 7}
	grant := state.Grant{Handle: cleanupHandle{object: request.Object}, Lease: state.Lease{ClientGUID: request.ClientGUID, Key: state.GUID{byte(inode & 0xff)}, State: rights, Epoch: 7}}
	if durable {
		request.CreateGUID = grant.Lease.Key
		grant.DurableTimeout = time.Minute
	}
	return commitLeaseOpen(t, server, request, grant)
}

func commitLeaseOpen(t *testing.T, server *Server, request state.OpenRequest, grant state.Grant) state.Open {
	t.Helper()
	reservation, status := server.options.State.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(reservation, grant)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

func startServerBreak(ctx context.Context, server *Server, open state.Open, target uint32) <-chan error {
	done := make(chan error, 1)
	go func() { done <- server.BreakLeases(ctx, open.Object, state.GUID{9}, state.GUID{9}, target) }()
	return done
}

func finishServerBreak(ctx context.Context, t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func acknowledgeBreak(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, key state.GUID, rights uint32) wire.Message {
	t.Helper()
	header := wire.Header{SessionID: session.SessionID, MessageID: id, CreditCharge: 1, Credit: 1}
	if err := client.SendLeaseBreakAcknowledgment(ctx, header, wire.LeaseBreakRequest{Key: [16]byte(key), State: rights}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("ack reply count: %d", len(response.Messages))
	}
	return response.Messages[0]
}

func TestLeaseBreakNotificationAndAcknowledgment(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES128GCM, smb.CipherAES256GCM} {
		for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
			for _, current := range []uint32{smb.LeaseRead, smb.LeaseRead | smb.LeaseHandle | smb.LeaseWrite} {
				t.Run(fmt.Sprintf("cipher_%d_signing_%d_current_%d", cipher, signing, current), func(t *testing.T) {
					checkLeaseBreakNotification(t, cipher, signing, current)
				})
			}
		}
	}
}

func checkLeaseBreakNotification(t *testing.T, cipher, signing uint16, current uint32) {
	t.Helper()
	server, client, ctx, session, _ := leaseServer(t, cipher, signing)
	open := insertLeaseOpen(t, server, session, 2, current, false)
	target, flags := uint32(0), uint32(0)
	if current != smb.LeaseRead {
		target, flags = smb.LeaseRead|smb.LeaseHandle, 1
	}
	done := startServerBreak(ctx, server, open, target)
	notification, err := client.WaitLeaseBreak(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.LeaseBreakNotification{Key: [16]byte(open.LeaseKey), CurrentState: current, NewState: target, Epoch: 8, Flags: flags}
	if notification != want {
		t.Fatalf("notification: %+v, want %+v", notification, want)
	}
	if flags != 0 {
		select {
		case err := <-done:
			t.Fatalf("wait ended before ack: %v", err)
		default:
		}
		response := acknowledgeBreak(ctx, t, client, session, session.NextMessageID, open.LeaseKey, target)
		ack, decodeErr := wire.DecodeLeaseBreakResponse(response)
		if decodeErr != nil || ack.Key != notification.Key || ack.State != target || ack.Duration != 0 || ack.Flags != 0 {
			t.Fatalf("ack response: %+v, %v", response, decodeErr)
		}
	}
	finishServerBreak(ctx, t, done)
}

func TestLeaseBreakAcknowledgmentRejectsInvalidRequests(t *testing.T) {
	server, client, ctx, session, _ := leaseServer(t, smb.CipherAES128GCM, smb.SigningGMAC)
	otherClient, _, other := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	wrongClient, wrongCtx := pipeClient(t, server)
	wrongSession, loginErr := wrongClient.Login(wrongCtx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{3}})
	if loginErr != nil {
		t.Fatal(loginErr)
	}
	open := insertLeaseOpen(t, server, session, 2, smb.LeaseRead|smb.LeaseWrite|smb.LeaseHandle, false)
	done := startServerBreak(ctx, server, open, smb.LeaseRead)
	if _, err := client.WaitLeaseBreak(ctx); err != nil {
		t.Fatal(err)
	}
	for index, test := range []struct {
		key    state.GUID
		rights uint32
		status smb.Status
	}{
		{key: state.GUID{99}, rights: smb.LeaseRead, status: smb.StatusObjectNameNotFound},
		{key: open.LeaseKey, rights: smb.LeaseRead | smb.LeaseHandle, status: smb.StatusRequestNotAccepted},
	} {
		response := acknowledgeBreak(ctx, t, client, session, session.NextMessageID+uint64(index), test.key, test.rights)
		if response.Header.Status != test.status || !server.options.State.BreakPending(state.Break{ClientGUID: open.ClientGUID, LeaseKey: open.LeaseKey}) {
			t.Fatalf("invalid ack changed break: %+v", response.Header)
		}
	}
	response := acknowledgeBreak(ctx, t, wrongClient, wrongSession, wrongSession.NextMessageID, open.LeaseKey, smb.LeaseRead)
	if response.Header.Status != smb.StatusObjectNameNotFound || !server.options.State.BreakPending(state.Break{ClientGUID: open.ClientGUID, LeaseKey: open.LeaseKey}) {
		t.Fatal("another client acknowledged or changed the lease")
	}
	response = acknowledgeBreak(ctx, t, otherClient, other, other.NextMessageID, open.LeaseKey, smb.LeaseRead)
	if response.Header.Status != smb.StatusInvalidParameter {
		t.Fatalf("another session acknowledged lease: %+v", response.Header)
	}
	response = acknowledgeBreak(ctx, t, client, session, session.NextMessageID+2, open.LeaseKey, smb.LeaseRead)
	if response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	finishServerBreak(ctx, t, done)
	response = acknowledgeBreak(ctx, t, client, session, session.NextMessageID+3, open.LeaseKey, smb.LeaseRead)
	if response.Header.Status != smb.StatusUnsuccessful {
		t.Fatalf("no pending break: %+v", response.Header)
	}
}

func TestDetachedLeaseBreakRetainingHCompletesWithoutNotification(t *testing.T) {
	server, client, ctx, session, storage := leaseServer(t, 0, smb.SigningCMAC)
	open := insertLeaseOpen(t, server, session, 2, smb.LeaseRead|smb.LeaseWrite|smb.LeaseHandle, true)
	joinDurableLease(t, server, session, open)
	if actions := server.options.State.Disconnect(session.SessionID); len(actions) != 0 {
		t.Fatal("durable open closed on disconnect")
	}
	if err := server.BreakLeases(ctx, open.Object, state.GUID{9}, state.GUID{9}, smb.LeaseRead|smb.LeaseHandle); err != nil {
		t.Fatal(err)
	}
	if storage.closed.Load() != 2 || server.options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
		t.Fatal("detached break kept rights or handle")
	}
	if err := client.Send(ctx, []wire.Message{sessionEcho(t, session, session.NextMessageID)}); err != nil {
		t.Fatal(err)
	}
	payload, err := client.ReceiveRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := wire.Split(payload)
	if err != nil || len(messages) != 1 || messages[0].Header.Command != wire.Echo {
		t.Fatalf("unexpected notification: %+v, %v", messages, err)
	}
}

func TestLeaseBreakWaitTimeoutAndCancellation(t *testing.T) {
	for _, finish := range []string{"timeout", "cancel", "timeout without reading", "cancel without reading"} {
		t.Run(finish, func(t *testing.T) { checkLeaseBreakWait(t, finish) })
	}
}

func checkLeaseBreakWait(t *testing.T, finish string) {
	t.Helper()
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
	var nanos atomic.Int64
	nanos.Store(time.Now().UnixNano())
	options.Now = func() time.Time { return time.Unix(0, nanos.Load()) }
	var err error
	options.State, err = state.New(options.Now)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	open := insertLeaseOpen(t, server, session, 2, smb.LeaseRead|smb.LeaseHandle, false)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	changed := options.State.BreakChanges()
	done := startServerBreak(waitCtx, server, open, smb.LeaseRead)
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if finish == "timeout" || finish == "cancel" {
		if _, receiveErr := client.WaitLeaseBreak(ctx); receiveErr != nil {
			t.Fatal(receiveErr)
		}
	}
	if finish == "cancel" || finish == "cancel without reading" {
		cancel()
		select {
		case waitErr := <-done:
			if !errors.Is(waitErr, context.Canceled) || !options.State.LeasesBreaking(open.Object, state.GUID{9}, state.GUID{9}) {
				t.Fatalf("cancel lost pending break: %v", waitErr)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return
	}
	nanos.Add(int64(state.LeaseBreakTimeout))
	if actions := options.State.ExpireBreaks(); len(actions) != 0 {
		t.Fatal("expiry closed an attached open")
	}
	finishServerBreak(ctx, t, done)
}

func TestClassicOplockAcknowledgment(t *testing.T) {
	server, client, ctx, session, _ := leaseServer(t, smb.CipherAES128GCM, smb.SigningCMAC)
	open := insertSessionOpen(t, server, session, false, 2)
	for index, test := range []struct {
		id     state.FileID
		treeID uint32
		status smb.Status
	}{
		{id: open.ID, treeID: session.TreeID, status: smb.StatusInvalidDeviceState},
		{id: state.FileID{Persistent: 99, Volatile: 99}, treeID: session.TreeID, status: smb.StatusFileClosed},
		{id: open.ID, treeID: session.TreeID + 1, status: smb.StatusNetworkNameDeleted},
	} {
		body, err := wire.EncodeOplockBreakRequest(wire.OplockBreakRequest{ID: wire.FileID{Persistent: test.id.Persistent, Volatile: test.id.Volatile}})
		if err != nil {
			t.Fatal(err)
		}
		message := wire.Message{Header: wire.Header{Command: wire.OplockBreak, SessionID: session.SessionID, TreeID: test.treeID, MessageID: session.NextMessageID + uint64(index), CreditCharge: 1, Credit: 1}, Body: body}
		response := exchange(ctx, t, client, message)[0]
		if response.Header.Status != test.status {
			t.Fatalf("classic ack: %+v", response.Header)
		}
	}
	if response := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+3))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("classic ack broke the connection")
	}
}
