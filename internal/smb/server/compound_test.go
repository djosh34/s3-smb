package server

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestEchoCompoundFraming(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 3))
	second := echo(t, 2)
	second.Header.Flags = wire.FlagRelated
	second.Header.SessionID, second.Header.TreeID = ^uint64(0), ^uint32(0)
	messages := exchange(ctx, t, client, echo(t, 1), second, echo(t, 3))
	if len(messages) != 3 {
		t.Fatalf("reply members: %d", len(messages))
	}
	for index, message := range messages {
		if message.Header.Status != smb.StatusSuccess || message.Header.MessageID != uint64(index+1) || message.Header.SessionID != 0 || message.Header.TreeID != 0 {
			t.Fatalf("member %d: %+v", index, message.Header)
		}
		if index < 2 && message.Header.NextCommand != 72 || index == 2 && message.Header.NextCommand != 0 {
			t.Fatalf("NextCommand: %d", message.Header.NextCommand)
		}
		if _, err := wire.DecodeEchoResponse(message); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBadCompoundBodyDoesNotDispatchPrefix(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		calls.Add(1)
		return handleEcho(ctx, request, message)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	bad := echo(t, 2)
	bad.Body = []byte{0, 0, 0, 0}
	messages := exchange(ctx, t, client, echo(t, 1), bad)
	if len(messages) != 2 || calls.Load() != 0 {
		t.Fatalf("bad compound dispatched %d handlers", calls.Load())
	}
	for _, message := range messages {
		if message.Header.Status != smb.StatusInvalidParameter {
			t.Fatalf("bad member: %+v", message.Header)
		}
	}
	messages = exchange(ctx, t, client, echo(t, 3))
	if messages[0].Header.Status != smb.StatusSuccess {
		t.Fatal("bad body dropped the connection")
	}
}

func TestMalformedCompoundLinkClosesConnection(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 2))
	payload, err := wire.Join([]wire.Message{echo(t, 1), echo(t, 2)})
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(payload[20:], 73)
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, 140)
	if err := client.SendRaw(ctx, append(frame, payload...)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receive(ctx); err == nil {
		t.Fatal("malformed compound received a reply")
	}
}

func TestAsyncRelatedSuffixGetsSeparatePendingIdentities(t *testing.T) {
	server, release := controlledAsync(t, wire.Read, reply{status: smb.StatusFileLockConflict}, nil)
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 4))
	suffix := echo(t, 3)
	suffix.Header.Flags, suffix.Header.SessionID, suffix.Header.TreeID = wire.FlagRelated, ^uint64(0), ^uint32(0)
	if err := client.Send(ctx, []wire.Message{echo(t, 1), asyncMessage(t, wire.Read, 2), suffix, echo(t, 4)}); err != nil {
		t.Fatal(err)
	}
	pendingIDs := make(map[uint64]uint64)
	for index, wantID := range []uint64{1, 2, 3, 4} {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 || response.Messages[0].Header.MessageID != wantID {
			t.Fatalf("prefix resent or response order changed: %+v", response.Messages)
		}
		header := response.Messages[0].Header
		if index == 1 || index == 2 {
			if header.Status != smb.StatusPending || header.SessionID != 77 {
				t.Fatalf("dependent pending: %+v", header)
			}
			pendingIDs[wantID] = header.AsyncID
		} else if header.Status != smb.StatusSuccess {
			t.Fatalf("independent ECHO: %+v", header)
		}
	}
	if pendingIDs[2] == pendingIDs[3] {
		t.Fatal("related members share an async identity")
	}
	close(release)
	seen := make(map[uint64]bool)
	for range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		header := response.Messages[0].Header
		want := smb.StatusFileLockConflict
		if header.MessageID == 3 {
			want = smb.StatusInvalidParameter
		}
		if header.MessageID != 2 && header.MessageID != 3 || seen[header.MessageID] || header.Status != want || header.Credit != 0 {
			t.Fatalf("dependent completion: %+v", header)
		}
		seen[header.MessageID] = true
	}
}
