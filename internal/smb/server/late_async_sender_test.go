package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The async producer reports send failures through the server logger.
type asyncReplyErrors chan error

func (asyncReplyErrors) Enabled(context.Context, slog.Level) bool { return true }
func (logs asyncReplyErrors) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "async reply failed" {
		return nil
	}
	replyErr := errors.New("async failure log lacks an error")
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "error" {
			if err, ok := attr.Value.Any().(error); ok {
				replyErr = err
			}
		}
		return true
	})
	logs <- replyErr
	return nil
}
func (logs asyncReplyErrors) WithAttrs([]slog.Attr) slog.Handler { return logs }
func (logs asyncReplyErrors) WithGroup(string) slog.Handler      { return logs }

func controlledAsyncExchange(ctx context.Context, t *testing.T, client *smbtest.Client, conn *controlledConn, message wire.Message) wire.Message {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	call := waitWrite(ctx, t, conn)
	call.release <- nil
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("controlled exchange returned %d replies", len(response.Messages))
	}
	return response.Messages[0]
}

// Hold the failing write until the async producer is waiting on its own queued
// send. Queue observation is only a scheduling barrier; assertions use the
// producer's error, transport bytes and ServeConn's return.
func waitAsyncSendQueued(ctx context.Context, t *testing.T, server *Server) {
	t.Helper()
	server.mu.Lock()
	var active *connection
	for conn := range server.connections {
		active = conn
	}
	server.mu.Unlock()
	if active == nil {
		t.Fatal("no active connection")
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		active.sender.mu.Lock()
		queued := len(active.sender.queue) > 0
		active.sender.mu.Unlock()
		if queued {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

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
