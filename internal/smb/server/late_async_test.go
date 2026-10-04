package server

import (
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestLateAsyncCompletionDuringShutdown(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "waits for late work"
		if deadline {
			name = "caller deadline"
		}
		t.Run(name, func(t *testing.T) {
			testLateAsyncShutdown(t, deadline)
		})
	}
}

func TestLateAsyncCompletionAfterConnectionClose(t *testing.T) {
	options := testOptions(t)
	logs := make(asyncReplyErrors, 4)
	options.Logger = slog.New(logs)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := newAsyncGate()
	canceled := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		<-ctx.Done()
		close(canceled)
		<-release.done // Deliberately ignore cancellation until the test releases work.
		return reply{status: smb.StatusFileLockConflict}, nil
	}
	local, remote := net.Pipe()
	conn := &observedAsyncConn{Conn: local, closed: make(chan struct{})}
	peer := serveLateAsyncPipe(t, server, conn, remote)
	t.Cleanup(release.release)
	exchange(peer.ctx, t, peer.client, negotiateMessage(t, 1))
	pending := exchange(peer.ctx, t, peer.client, asyncMessage(t, wire.Read, 1))[0]
	if pending.Header.Status != smb.StatusPending {
		t.Fatalf("request did not become pending: %+v", pending.Header)
	}
	if err := peer.client.Close(); err != nil {
		t.Fatal(err)
	}
	waitAsyncSignal(peer.ctx, t, canceled)
	waitAsyncSignal(peer.ctx, t, conn.closed)
	select {
	case <-peer.done:
		t.Fatal("ServeConn returned before the handler finished")
	default:
	}
	writes := conn.writes.Load()
	release.release()
	waitAsyncSignal(peer.ctx, t, peer.done)
	if peer.err != nil {
		t.Fatalf("peer-close cleanup: %v", peer.err)
	}
	if conn.writes.Load() != writes || conn.lateWrites.Load() != 0 {
		t.Fatalf("late reply attempted a write: before %d, after %d, closed writes %d", writes, conn.writes.Load(), conn.lateWrites.Load())
	}
	assertNoAsyncFailure(t, logs)
}
