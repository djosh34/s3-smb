package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestPreviousSessionIDReplacesLiveSessionAndDetachesDurability(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		<-ctx.Done()
		close(drained)
		return reply{}, ctx.Err()
	}
	oldClient, oldCtx, old := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	ordinary := insertSessionOpen(t, server, old, false, 21)
	durable := insertSessionOpen(t, server, old, true, 22)
	pending := exchange(oldCtx, t, oldClient, treeRequest(t, old, old.NextMessageID, wire.Read))[0]
	if pending.Header.Status != smb.StatusPending {
		t.Fatal(pending.Header)
	}
	client, ctx := pipeClient(t, server)
	fresh, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningCMAC, ClientGUID: [16]byte{2}, PreviousSessionID: old.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	default:
		t.Fatal("previous session work was not drained")
	}
	if storage.closed.Load() != 1 {
		t.Fatalf("replacement closed %d handles, want one ordinary handle", storage.closed.Load())
	}
	for _, open := range []state.Open{ordinary, durable} {
		if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
			t.Fatal("old open remains attached")
		}
	}
	attached, status := options.State.Reconnect(state.ReconnectRequest{ID: durable.ID, Binding: state.Binding{SessionID: fresh.SessionID, TreeID: fresh.TreeID}, User: durable.User, Share: durable.Share, ClientGUID: durable.ClientGUID, CreateGUID: durable.CreateGUID, LeaseKey: durable.LeaseKey})
	if status != smb.StatusSuccess || attached.ID.Volatile == durable.ID.Volatile {
		t.Fatal("replacement closed durability instead of detaching it")
	}
	final, err := oldClient.Receive(oldCtx)
	if err != nil {
		t.Fatal(err)
	}
	if final.Messages[0].Header.Status != smb.StatusCancelled || final.Messages[0].Header.Credit != 0 {
		t.Fatal("old async reply lost its keys or identity")
	}
	// Session replacement removes an identity, not its still-live transport.
	assertRawStatus(oldCtx, t, oldClient, echo(t, old.NextMessageID+1), smb.StatusSuccess)
	assertRawStatus(oldCtx, t, oldClient, sessionEcho(t, old, old.NextMessageID+2), smb.StatusUserSessionDeleted)
	if response := exchange(ctx, t, client, sessionEcho(t, fresh, fresh.NextMessageID))[0]; response.Header.Status != smb.StatusSuccess {
		t.Fatal("new session is not usable")
	}
}

func assertRawStatus(ctx context.Context, t *testing.T, client *smbtest.Client, message wire.Message, status smb.Status) {
	t.Helper()
	payload, err := wire.Join([]wire.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(ctx, t, client, payload)
	response, err := client.ReceiveRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members, err := wire.Split(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Header.Status != status {
		t.Fatalf("raw status: %+v, want %#x", members, status)
	}
}

func TestPreviousSessionIDCanReplaceSessionOnSameConnection(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newRawSessions(t, server)
	old := client.start(t, 0)
	client.finish(t, old, 0)
	fresh := client.start(t, 0)
	protector := client.finish(t, fresh, old.id)
	owner := onlyConnection(t, server)
	owner.sessionMu.RLock()
	_, retainedOld := owner.sessions[old.id]
	count := len(owner.sessions)
	owner.sessionMu.RUnlock()
	if retainedOld || count != 1 {
		t.Fatal("same-connection replacement retained old identity")
	}
	if response := client.protected(t, protector, sessionEcho(t, smbtest.Session{SessionID: fresh.id}, 0)); response.Header.Status != smb.StatusSuccess {
		t.Fatal("new session has stale keys")
	}
}

func TestPreviousSessionIDIgnoresMissingSelfAndDifferentUser(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	owner := &connection{server: server, sessions: map[uint64]*sessionEntry{1: {identity: Session{SessionID: 1, User: "other"}, active: true}}}
	server.registerSession(1, owner)
	for _, previousID := range []uint64{0, 1, 99} {
		if err := server.replaceSession(t.Context(), 2, previousID, "backup"); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.replaceSession(t.Context(), 1, 1, "other"); err != nil {
		t.Fatal(err)
	}
	if owner.sessions[1] == nil || !owner.sessions[1].active || server.sessions[1] != owner {
		t.Fatal("previous ID removed an unrelated or self session")
	}
}
