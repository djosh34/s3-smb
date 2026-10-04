package server

import (
	"errors"
	"io"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestDH2CReattachesRetainedOpen(t *testing.T) {
	server, client, ctx, session := newFileClient(t)
	options := durableCreateOptions()
	first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
	binding := state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}
	original, status := server.options.State.Find(state.FileID(first.Reply.ID), binding)
	if status != smb.StatusSuccess {
		t.Fatalf("find = %#x", status)
	}
	if lockStatus := server.options.State.Lock(original.ID, binding, []state.Range{{Length: 10, Exclusive: true}}, false); lockStatus != smb.StatusSuccess {
		t.Fatalf("lock = %#x", lockStatus)
	}
	// Keep the old transport live. SESSION_SETUP must detach its durable open.
	newClient, newCtx := pipeClient(t, server)
	newSession, err := newClient.Login(newCtx, smbtest.LoginOptions{
		Share: server.options.ShareName, Account: server.options.Account,
		ClientGUID: session.ClientGUID, PreviousSessionID: session.SessionID, Cipher: session.Cipher, Signing: session.Signing,
	})
	if err != nil {
		t.Fatal(err)
	}
	options.Reconnect = &wire.DurableReconnect{ID: first.Reply.ID, CreateGUID: options.Durable.CreateGUID}
	options.Durable = nil
	second := durableResult(t, durableExchange(newCtx, t, newClient, newSession, newSession.NextMessageID, options))
	if second.Reply.ID.Persistent != first.Reply.ID.Persistent || second.Reply.ID.Volatile == first.Reply.ID.Volatile || second.Reply.Action != 1 || second.Durable == nil || second.Lease == nil || *second.Lease != *first.Lease {
		t.Fatalf("reconnect changed identity or grant: first %+v, second %+v", first, second)
	}
	newBinding := state.Binding{SessionID: newSession.SessionID, TreeID: newSession.TreeID}
	reattached, status := server.options.State.Find(state.FileID(second.Reply.ID), newBinding)
	if status != smb.StatusSuccess || reattached.Handle != original.Handle || reattached.GrantedAccess != original.GrantedAccess || reattached.Sharing != original.Sharing {
		t.Fatalf("reattached = %+v, status %#x", reattached, status)
	}
	if status := server.options.State.Lock(reattached.ID, newBinding, []state.Range{{Length: 10}}, true); status != smb.StatusSuccess {
		t.Fatalf("retained range unlock = %#x", status)
	}
	stale := fileClose(newCtx, t, newClient, newSession, newSession.NextMessageID+1, first.Reply.ID, 0)
	if stale.Header.Status != smb.StatusFileClosed {
		t.Fatalf("old volatile ID = %#x", stale.Header.Status)
	}
	if err := client.Send(ctx, []wire.Message{sessionEcho(t, session, session.NextMessageID+1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("old encryption keys were not rejected: %v", err)
	}
	closeReply := fileClose(newCtx, t, newClient, newSession, newSession.NextMessageID+2, second.Reply.ID, 0)
	if closeReply.Header.Status != smb.StatusSuccess {
		t.Fatalf("new ID CLOSE = %#x", closeReply.Header.Status)
	}
}

func TestDH2CRejectsMismatchedContextsAndIdentities(t *testing.T) {
	for _, test := range []struct {
		modify func(*smbtest.CreateOptions)
		name   string
	}{
		{name: "persistent ID", modify: func(o *smbtest.CreateOptions) { o.Reconnect.ID.Persistent++ }},
		{name: "volatile ID", modify: func(o *smbtest.CreateOptions) { o.Reconnect.ID.Volatile++ }},
		{name: "create GUID", modify: func(o *smbtest.CreateOptions) { o.Reconnect.CreateGUID[0]++ }},
		{name: "lease key", modify: func(o *smbtest.CreateOptions) { o.Lease.Key[0]++ }},
		{name: "persistent flag", modify: func(o *smbtest.CreateOptions) { o.Reconnect.Flags = 2 }},
		{name: "missing lease", modify: func(o *smbtest.CreateOptions) { o.Lease = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			options := durableCreateOptions()
			first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
			if actions := server.options.State.Disconnect(session.SessionID); len(actions) != 0 {
				t.Fatalf("durable disconnect closed handles: %+v", actions)
			}
			good := smbtest.CreateOptions{Request: options.Request, Lease: first.Lease, Reconnect: &wire.DurableReconnect{ID: first.Reply.ID, CreateGUID: options.Durable.CreateGUID}}
			bad := good
			lease := *good.Lease
			reconnect := *good.Reconnect
			bad.Lease, bad.Reconnect = &lease, &reconnect
			test.modify(&bad)
			bad.Request.Name = "must-not-be-created"
			message := durableExchange(ctx, t, client, session, session.NextMessageID+1, bad)
			want := smb.StatusObjectNameNotFound
			if test.name == "persistent flag" {
				want = smb.StatusInvalidParameter
			}
			if message.Header.Status != want {
				t.Fatalf("mismatched %s = %#x", test.name, message.Header.Status)
			}
			resolved, err := server.options.Storage.Lookup(ctx, bad.Request.Name)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Exists {
				t.Fatal("failed reconnect created a name")
			}
			result := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID+2, good))
			if result.Reply.ID.Persistent != first.Reply.ID.Persistent {
				t.Fatal("mismatch consumed original open")
			}
		})
	}
}

// Seed a retained open with each identity mismatch. Requests still authenticate
// normally and use the real adapter; another user's or share's open is never
// eligible even if the client knows its complete file ID and CREATE GUID.
func TestDH2CRejectsOtherUserShareAndClient(t *testing.T) {
	for _, test := range []struct {
		modify func(*state.OpenRequest)
		name   string
	}{
		{name: "user", modify: func(r *state.OpenRequest) { r.User = "other" }},
		{name: "share", modify: func(r *state.OpenRequest) { r.Share = "other" }},
		{name: "client", modify: func(r *state.OpenRequest) { r.ClientGUID[0]++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			storage := server.options.Storage
			resolved, err := storage.Lookup(ctx, "retained")
			if err != nil {
				t.Fatal(err)
			}
			resolved, err = storage.Create(ctx, resolved.Name, smb.KindFile)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := storage.Open(ctx, resolved.Object, smb.AccessRead|smb.AccessWrite)
			if err != nil {
				t.Fatal(err)
			}
			req := state.OpenRequest{Object: resolved.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: session.ClientGUID, CreateGUID: [16]byte{5}, GrantedAccess: fileReadData, Sharing: 7}
			test.modify(&req)
			token, status := server.options.State.Reserve(req)
			if status != smb.StatusSuccess {
				t.Fatal(status)
			}
			lease := state.Lease{ClientGUID: req.ClientGUID, Key: [16]byte{4}, State: smb.LeaseRead | smb.LeaseHandle}
			open, status := server.options.State.Commit(token, state.Grant{Handle: handle, Lease: lease, DurableTimeout: smb.DefaultDurableTimeout})
			if status != smb.StatusSuccess {
				t.Fatal(status)
			}
			server.options.State.Disconnect(session.SessionID)
			options := durableCreateOptions()
			options.Durable = nil
			options.Reconnect = &wire.DurableReconnect{ID: wire.FileID(open.ID), CreateGUID: open.CreateGUID}
			message := durableExchange(ctx, t, client, session, session.NextMessageID, options)
			want := smb.StatusObjectNameNotFound
			if test.name == "user" {
				want = smb.StatusAccessDenied
			}
			if message.Header.Status != want {
				t.Fatalf("other %s reconnect = %#x", test.name, message.Header.Status)
			}
		})
	}
}
