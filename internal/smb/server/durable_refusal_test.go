package server

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestDH2CRejectsAttachedAndExpiredOpens(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "attached"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			var now atomic.Int64
			now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
			clock := func() time.Time { return time.Unix(0, now.Load()) }
			options := testOptions(t)
			options.Storage = newFilesMetaStorage(t)
			options.Now = clock
			table, err := state.New(clock)
			if err != nil {
				t.Fatal(err)
			}
			options.State = table
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
			create := durableCreateOptions()
			create.Durable.Timeout = 1
			first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, create))
			if expired {
				table.Disconnect(session.SessionID)
				now.Add(int64(time.Millisecond))
			}
			create.Reconnect = &wire.DurableReconnect{ID: first.Reply.ID, CreateGUID: create.Durable.CreateGUID}
			create.Durable = nil
			message := durableExchange(ctx, t, client, session, session.NextMessageID+1, create)
			if message.Header.Status != smb.StatusObjectNameNotFound {
				t.Fatalf("%s DH2C = %#x", name, message.Header.Status)
			}
		})
	}
}

func TestDH2CRejectsLegacyAndMissingLeaseSemantics(t *testing.T) {
	for _, name := range []string{"lease v1", "oplock none", "legacy request", "legacy reconnect"} {
		t.Run(name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			options := durableCreateOptions()
			first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
			server.options.State.Disconnect(session.SessionID)
			lease := *first.Lease
			if name == "lease v1" {
				lease.Version, lease.Epoch = 1, 0
			}
			leaseContext, err := wire.EncodeLeaseContext(lease)
			if err != nil {
				t.Fatal(err)
			}
			reconnect, err := wire.EncodeDurableReconnect(wire.DurableReconnect{ID: first.Reply.ID, CreateGUID: options.Durable.CreateGUID})
			if err != nil {
				t.Fatal(err)
			}
			create := options.Request
			create.OplockLevel = leaseOplockLevel
			create.Contexts = []wire.CreateContext{leaseContext, reconnect}
			switch name {
			case "oplock none":
				create.OplockLevel = 0
			case "legacy request":
				create.Contexts = append(create.Contexts, wire.CreateContext{Name: "DHnQ", Data: make([]byte, 16)})
			case "legacy reconnect":
				create.Contexts = append(create.Contexts, wire.CreateContext{Name: "DHnC", Data: make([]byte, 16)})
			}
			message := fileCreate(ctx, t, client, session, session.NextMessageID+1, create)
			if message.Header.Status != smb.StatusObjectNameNotFound {
				t.Fatalf("%s context = %#x", name, message.Header.Status)
			}
			options.Reconnect = &wire.DurableReconnect{ID: first.Reply.ID, CreateGUID: options.Durable.CreateGUID}
			options.Durable = nil
			good := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID+2, options))
			if good.Reply.ID.Volatile != first.Reply.ID.Volatile+1 {
				t.Fatal("refused context changed retained open")
			}
		})
	}
}

func TestDurableV1ReconnectIsRefusedWithoutCreating(t *testing.T) {
	server, client, ctx, session := newFileClient(t)
	message := fileCreate(ctx, t, client, session, session.NextMessageID, wire.CreateRequest{Name: "not-created", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf, Contexts: []wire.CreateContext{{Name: "DHnC", Data: make([]byte, 16)}}})
	if message.Header.Status != smb.StatusObjectNameNotFound {
		t.Fatalf("durable v1 reconnect = %#x", message.Header.Status)
	}
	resolved, err := server.options.Storage.Lookup(ctx, "not-created")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Exists {
		t.Fatal("durable v1 reconnect created a new file")
	}
}
