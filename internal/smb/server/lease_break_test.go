package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

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
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("same-client second session ACK = %+v", response.Header)
	}
	finishServerBreak(ctx, t, done)
	response = acknowledgeBreak(ctx, t, client, session, session.NextMessageID+2, open.LeaseKey, smb.LeaseRead)
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
