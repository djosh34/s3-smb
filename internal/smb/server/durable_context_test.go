package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestDurableContextsFailBeforeNamespaceMutation(t *testing.T) {
	request, err := wire.EncodeDurableRequest(wire.DurableRequest{CreateGUID: [16]byte{5}})
	if err != nil {
		t.Fatal(err)
	}
	reconnect, err := wire.EncodeDurableReconnect(wire.DurableReconnect{CreateGUID: [16]byte{5}})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := wire.EncodeLeaseContext(wire.LeaseContext{Version: 2, Key: [16]byte{4}, State: smb.LeaseRead | smb.LeaseHandle})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		contexts []wire.CreateContext
	}{
		{name: "short request", contexts: []wire.CreateContext{{Name: "DH2Q", Data: []byte{1}}}},
		{name: "short reconnect", contexts: []wire.CreateContext{{Name: "DH2C", Data: []byte{1}}}},
		{name: "short lease", contexts: []wire.CreateContext{{Name: "RqLs", Data: []byte{1}}, request}},
		{name: "duplicate request", contexts: []wire.CreateContext{request, request}},
		{name: "duplicate reconnect", contexts: []wire.CreateContext{reconnect, reconnect}},
		{name: "request and reconnect", contexts: []wire.CreateContext{request, reconnect}},
		{name: "reconnect and request", contexts: []wire.CreateContext{reconnect, request}},
		{name: "duplicate lease", contexts: []wire.CreateContext{lease, lease, request}},
		{name: "zero create GUID", contexts: []wire.CreateContext{{Name: "DH2Q", Data: make([]byte, 32)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			message := fileCreate(ctx, t, client, session, session.NextMessageID, wire.CreateRequest{Name: "must-not-be-created", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf, Contexts: test.contexts})
			if message.Header.Status != smb.StatusInvalidParameter {
				t.Fatalf("context status = %#x", message.Header.Status)
			}
			resolved, err := server.options.Storage.Lookup(ctx, "must-not-be-created")
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Exists {
				t.Fatal("invalid contexts mutated namespace")
			}
			if echo := exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+1))[0]; echo.Header.Status != smb.StatusSuccess {
				t.Fatalf("ECHO after invalid context = %#x", echo.Header.Status)
			}
		})
	}
}

func TestUnknownMarkedReplayExecutesCreate(t *testing.T) {
	_, client, ctx, session := newFileClient(t)
	options := durableCreateOptions()
	options.Replay = true
	result := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
	if result.Reply.Action != 2 || result.Durable == nil || result.Lease == nil {
		t.Fatalf("unknown replay did not execute CREATE: %+v", result)
	}
}

func TestReplayContextOrderDoesNotChangeParameters(t *testing.T) {
	_, client, ctx, session := newFileClient(t)
	options := durableCreateOptions()
	first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
	lease, err := wire.EncodeLeaseContext(*options.Lease)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := wire.EncodeDurableRequest(*options.Durable)
	if err != nil {
		t.Fatal(err)
	}
	create := options.Request
	create.OplockLevel = 0xff
	create.Contexts = []wire.CreateContext{durable, lease} // Reverse the typed client's order.
	body, err := wire.EncodeCreateRequest(create)
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.Create, Flags: wire.FlagReplay, SessionID: session.SessionID, TreeID: session.TreeID, MessageID: session.NextMessageID + 1, CreditCharge: 1, Credit: 1}, Body: body}
	result := durableResult(t, exchange(ctx, t, client, message)[0])
	if result.Reply.ID != first.Reply.ID {
		t.Fatal("reordered contexts created another open")
	}
}
