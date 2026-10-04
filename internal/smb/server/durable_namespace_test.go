package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestDH2CFollowsRenameAndRejectsStaleOrReplacedNames(t *testing.T) {
	for _, test := range []struct {
		name        string
		replacement bool
		stale       bool
	}{
		{name: "current name"},
		{name: "stale name", stale: true},
		{name: "replacement inode", stale: true, replacement: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			options := durableCreateOptions()
			first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
			original := durableOpen(t, server, session, first.Reply.ID)
			server.options.State.Disconnect(session.SessionID)
			source, err := server.options.Storage.Lookup(ctx, options.Request.Name)
			if err != nil {
				t.Fatal(err)
			}
			destination, err := server.options.Storage.Lookup(ctx, "renamed")
			if err != nil {
				t.Fatal(err)
			}
			if err := server.options.Storage.Rename(ctx, smb.RenameRequest{Source: source.Name, Destination: destination.Name, SourceInode: source.Object.Inode}); err != nil {
				t.Fatal(err)
			}
			if test.replacement {
				if _, err := server.options.Storage.Create(ctx, source.Name, smb.KindFile); err != nil {
					t.Fatal(err)
				}
			}
			reconnect := smbtest.CreateOptions{Request: options.Request, Lease: first.Lease, Reconnect: &wire.DurableReconnect{ID: first.Reply.ID, CreateGUID: options.Durable.CreateGUID}}
			id := session.NextMessageID + 1
			if test.stale {
				message := durableExchange(ctx, t, client, session, id, reconnect)
				if message.Header.Status != smb.StatusObjectNameNotFound {
					t.Fatalf("stale name = %#x", message.Header.Status)
				}
				id++
			}
			reconnect.Request.Name = "renamed"
			result := durableResult(t, durableExchange(ctx, t, client, session, id, reconnect))
			reattached, status := server.options.State.Find(state.FileID(result.Reply.ID), original.Binding)
			if status != smb.StatusSuccess || reattached.Object != original.Object || reattached.Handle != original.Handle || reattached.ID.Volatile != original.ID.Volatile+1 {
				t.Fatalf("rename/rejected path changed retained open: %+v, %#x", reattached, status)
			}
		})
	}
}

func TestDH2CDeleteOnCloseSkipsFilenameAndKeepsDeletion(t *testing.T) {
	server, client, ctx, session := newFileClient(t)
	options := durableCreateOptions()
	options.Request.Options |= fileDeleteOnClose
	options.Request.DesiredAccess = fileAllAccess
	first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
	server.options.State.Disconnect(session.SessionID)
	options.Reconnect = &wire.DurableReconnect{ID: first.Reply.ID, CreateGUID: options.Durable.CreateGUID}
	options.Durable = nil
	options.Request.Name = "not-the-retained-name"
	second := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID+1, options))
	closeReply := fileClose(ctx, t, client, session, session.NextMessageID+2, second.Reply.ID, 0)
	if closeReply.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE = %#x", closeReply.Header.Status)
	}
	for _, name := range []string{"durable", options.Request.Name} {
		resolved, err := server.options.Storage.Lookup(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Exists {
			t.Fatalf("delete-on-close reconnect left %q", name)
		}
	}
}
