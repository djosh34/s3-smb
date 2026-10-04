package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestRelatedCleanupDoesNotWaitForItsOwnCompletion(t *testing.T) {
	for _, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
		t.Run(fmt.Sprintf("command_%d", command), func(t *testing.T) { checkRelatedCleanup(t, command) })
	}
}

func checkRelatedCleanup(t *testing.T, command wire.Command) {
	t.Helper()
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
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
	open := insertSessionOpen(t, server, session, false, 12)
	read := treeRequest(t, session, session.NextMessageID, wire.Read)
	cleanup := treeRequest(t, session, session.NextMessageID+1, command)
	cleanup.Header.Flags = wire.FlagRelated
	cleanup.Header.SessionID, cleanup.Header.TreeID = ^uint64(0), ^uint32(0)
	if err := client.Send(ctx, []wire.Message{read, cleanup}); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		header := response.Messages[0].Header
		if header.Status != smb.StatusPending || header.MessageID != session.NextMessageID+uint64(index) {
			t.Fatal(header)
		}
	}
	close(release)
	seen := make(map[wire.Command]bool)
	for range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		header := response.Messages[0].Header
		if header.Status != smb.StatusSuccess || header.Credit != 0 || seen[header.Command] {
			t.Fatal(header)
		}
		seen[header.Command] = true
	}
	if !seen[wire.Read] || !seen[command] {
		t.Fatal("missing final response")
	}
	if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
		t.Fatal("related cleanup did not close the open")
	}
}

func TestSendRawBypassesLoginProtection(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	payload, err := wire.Join([]wire.Message{sessionEcho(t, session, session.NextMessageID)})
	if err != nil {
		t.Fatal(err)
	}
	sendPayload(ctx, t, client, payload)
	if _, err := client.ReceiveRaw(ctx); err == nil {
		t.Fatal("raw plaintext was encrypted by the client or accepted by the server")
	}
}
