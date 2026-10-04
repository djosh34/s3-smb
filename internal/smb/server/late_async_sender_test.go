package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLateAsyncCompletionRacesPartialWriteFailure(t *testing.T) {
	options := testOptions(t)
	logs := make(asyncReplyErrors, 4)
	options.Logger = slog.New(logs)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := newAsyncGate()
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		select {
		case <-release.done:
			return reply{status: smb.StatusFileLockConflict}, nil
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	local, remote := net.Pipe()
	conn := &controlledConn{Conn: local, writes: make(chan controlledWrite, 3)}
	peer := serveLateAsyncPipe(t, server, conn, remote)
	t.Cleanup(release.release)
	controlledAsyncExchange(peer.ctx, t, peer.client, conn, negotiateMessage(t, 2))
	pending := controlledAsyncExchange(peer.ctx, t, peer.client, conn, asyncMessage(t, wire.Read, 1))
	if pending.Header.Status != smb.StatusPending {
		t.Fatalf("request did not become pending: %+v", pending.Header)
	}
	if err := peer.client.Send(peer.ctx, []wire.Message{echo(t, 2)}); err != nil {
		t.Fatal(err)
	}
	call := waitWrite(peer.ctx, t, conn)
	writeErr := errors.New("controlled partial write during async completion")
	t.Cleanup(func() { call.release <- writeErr })
	release.release()
	waitAsyncSendQueued(peer.ctx, t, server)
	noCompletion(t, logs)
	call.release <- writeErr
	if _, err := peer.client.Receive(peer.ctx); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial frame followed by a reply: %v", err)
	}
	if err := waitAsyncResult(peer.ctx, t, logs); !errors.Is(err, writeErr) {
		t.Fatalf("async producer lost its sender error: %v", err)
	}
	waitAsyncSignal(peer.ctx, t, peer.done)
	if !errors.Is(peer.err, writeErr) {
		t.Fatalf("connection lost the partial write error: %v", peer.err)
	}
	if conn.count.Load() != 3 {
		t.Fatalf("sender wrote %d frames; want only NEGOTIATE, PENDING and the partial ECHO", conn.count.Load())
	}
	if err := peer.client.Send(peer.ctx, []wire.Message{echo(t, 3)}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failed sender kept the connection open: %v", err)
	}
}
