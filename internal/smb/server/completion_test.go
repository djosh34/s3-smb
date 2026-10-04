package server

import (
	"context"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestAsyncSuccessUsesFinalBodyAndNoCredits(t *testing.T) {
	body, err := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("complete")})
	if err != nil {
		t.Fatal(err)
	}
	server, release := controlledAsync(t, wire.Read, reply{body: body}, nil)
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	pending := exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))[0]
	close(release)
	final, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	response := final.Messages[0]
	decoded, err := wire.DecodeReadResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Data) != "complete" || response.Header.Credit != 0 || response.Header.AsyncID != pending.Header.AsyncID {
		t.Fatalf("async success: %+v %+v", response.Header, decoded)
	}
}

func TestCancelAffectsOnlyItsPendingRequest(t *testing.T) {
	server, release := controlledAsync(t, wire.Read, reply{status: smb.StatusFileLockConflict}, nil)
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	pending := exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))[0]
	exchange(ctx, t, client, asyncMessage(t, wire.Read, 2))
	body, err := wire.EncodeCancelRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cancel := wire.Message{Header: wire.Header{Command: wire.Cancel, MessageID: 1, SessionID: 77, Flags: wire.FlagAsync, AsyncID: pending.Header.AsyncID}, Body: body}
	if sendErr := client.Send(ctx, []wire.Message{cancel}); sendErr != nil {
		t.Fatal(sendErr)
	}
	if sendErr := client.Send(ctx, []wire.Message{echo(t, 3)}); sendErr != nil {
		t.Fatal(sendErr)
	}
	seen := make(map[uint64]bool)
	for range 2 {
		response, receiveErr := client.Receive(ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		header := response.Messages[0].Header
		want := smb.StatusSuccess
		if header.MessageID == 1 {
			want = smb.StatusCancelled
		}
		if header.MessageID != 1 && header.MessageID != 3 || seen[header.MessageID] || header.Status != want {
			t.Fatalf("cancel affected another request or replied: %+v", header)
		}
		seen[header.MessageID] = true
	}
	close(release)
	final, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if final.Messages[0].Header.MessageID != 2 || final.Messages[0].Header.Status != smb.StatusFileLockConflict {
		t.Fatalf("unrelated pending was canceled: %+v", final.Messages[0].Header)
	}
}

func TestShutdownCancelsPendingWorkAndDrains(t *testing.T) {
	server, _ := controlledAsync(t, wire.Read, reply{}, nil)
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	exchange(ctx, t, client, asyncMessage(t, wire.Read, 1))
	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown did not drain canceled work: %v", err)
	}
	if _, err := client.Receive(ctx); err == nil {
		t.Fatal("shutdown kept the transport open")
	}
}
