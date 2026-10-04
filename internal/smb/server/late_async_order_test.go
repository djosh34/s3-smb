package server

import (
	"context"
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLateAsyncCompletionsKeepTheirOwnIdentity(t *testing.T) {
	for _, simultaneous := range []bool{false, true} {
		name := "reverse order"
		if simultaneous {
			name = "simultaneous"
		}
		t.Run(name, func(t *testing.T) { testLateAsyncOrder(t, simultaneous) })
	}
}

func testLateAsyncOrder(t *testing.T, simultaneous bool) {
	t.Helper()
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	statuses := []smb.Status{smb.StatusFileLockConflict, smb.StatusAccessDenied, smb.StatusDiskFull}
	gates := []*asyncGate{newAsyncGate(), newAsyncGate(), newAsyncGate()}
	if simultaneous {
		gates[1], gates[2] = gates[0], gates[0]
	}
	server.handlers[wire.Read] = func(ctx context.Context, message wire.Message) (reply, error) {
		if message.Header.MessageID < 1 || message.Header.MessageID > 3 {
			return reply{}, errors.New("unexpected controlled request ID")
		}
		index := int(message.Header.MessageID) - 1
		select {
		case <-gates[index].done:
			return reply{status: statuses[index]}, nil
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	client, ctx := pipeClient(t, server)
	for _, gate := range gates {
		t.Cleanup(gate.release)
	}
	exchange(ctx, t, client, negotiateMessage(t, 3))
	pending := make(map[uint64]wire.Header)
	asyncIDs := make(map[uint64]bool)
	for id := uint64(1); id <= 3; id++ {
		header := exchange(ctx, t, client, asyncMessage(t, wire.Read, id))[0].Header
		if header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || asyncIDs[header.AsyncID] {
			t.Fatalf("pending request lost its own async identity: %+v", header)
		}
		pending[id] = header
		asyncIDs[header.AsyncID] = true
	}
	if simultaneous {
		gates[0].release()
	}
	seen := make(map[uint64]bool)
	for wantID := uint64(3); wantID > 0; wantID-- {
		if !simultaneous {
			gates[wantID-1].release()
		}
		final, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(final.Messages) != 1 {
			t.Fatalf("final frame contains %d replies", len(final.Messages))
		}
		header := final.Messages[0].Header
		interim, exists := pending[header.MessageID]
		if !exists || seen[header.MessageID] || !simultaneous && header.MessageID != wantID {
			t.Fatalf("unexpected completion: %+v", header)
		}
		assertLateAsyncIdentity(t, header, interim, statuses[header.MessageID-1])
		seen[header.MessageID] = true
	}
	// This also detects an extra final reply left ahead of unrelated traffic.
	response := exchange(ctx, t, client, echo(t, 4))
	if len(response) != 1 || response[0].Header.MessageID != 4 || response[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("completion leaked into the next exchange: %+v", response)
	}
}

func assertLateAsyncIdentity(t *testing.T, final, pending wire.Header, status smb.Status) {
	t.Helper()
	if final.MessageID != pending.MessageID || final.AsyncID != pending.AsyncID || final.SessionID != pending.SessionID || final.Command != pending.Command || final.Flags&wire.FlagAsync == 0 || final.Status != status || final.Credit != 0 || final.CreditCharge != pending.CreditCharge || final.TreeID != 0 {
		t.Fatalf("final reply lost identity, status or credits: final %+v, pending %+v, want status %v", final, pending, status)
	}
}
