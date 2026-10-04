package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestStopRequestsCancelsButDoesNotWaitForOtherCleanups(t *testing.T) {
	for _, pending := range []bool{true, false} {
		name := "identity_holders"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) { checkCleanupWaits(t, pending) })
	}
}

func TestConcurrentAsyncLogoffAndTreeDisconnectComplete(t *testing.T) {
	options := testOptions(t)
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		select {
		case <-release:
			body, encodeErr := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("x")})
			return reply{body: body}, encodeErr
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	insertSessionOpen(t, server, session, false, 23)
	for index, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
		id := session.NextMessageID + uint64(2*index)
		read := treeRequest(t, session, id, wire.Read)
		cleanup := treeRequest(t, session, id+1, command)
		cleanup.Header.Flags = wire.FlagRelated
		cleanup.Header.SessionID, cleanup.Header.TreeID = ^uint64(0), ^uint32(0)
		if err := client.Send(ctx, []wire.Message{read, cleanup}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			response, err := client.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if response.Messages[0].Header.Status != smb.StatusPending {
				t.Fatal(response.Messages[0].Header)
			}
		}
	}
	close(release)
	seen := make(map[uint64]bool)
	for range 4 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		header := response.Messages[0].Header
		if seen[header.MessageID] || header.MessageID < session.NextMessageID || header.MessageID >= session.NextMessageID+4 || header.Flags&wire.FlagAsync == 0 || header.Credit != 0 {
			t.Fatal("invalid or duplicate async cleanup completion")
		}
		seen[header.MessageID] = true
	}
	if storage.closed.Load() != 1 {
		t.Fatal("concurrent cleanups did not close the open exactly once")
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal("shutdown did not drain concurrent cleanup", err)
	}
}
