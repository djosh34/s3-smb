package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type asyncGate struct {
	done chan struct{}
	once sync.Once
}

func newAsyncGate() *asyncGate   { return &asyncGate{done: make(chan struct{})} }
func (gate *asyncGate) release() { gate.once.Do(func() { close(gate.done) }) }

// Observe transport writes, including attempts made after the server closes it.
type observedAsyncConn struct {
	net.Conn
	closeErr   error
	closed     chan struct{}
	closeOnce  sync.Once
	writes     atomic.Int32
	lateWrites atomic.Int32
}

func (conn *observedAsyncConn) Write(data []byte) (int, error) {
	conn.writes.Add(1)
	select {
	case <-conn.closed:
		conn.lateWrites.Add(1)
	default:
	}
	return conn.Conn.Write(data)
}

func (conn *observedAsyncConn) Close() error {
	conn.closeOnce.Do(func() {
		conn.closeErr = conn.Conn.Close()
		close(conn.closed)
	})
	return conn.closeErr
}

type lateAsyncPeer struct {
	client *smbtest.Client
	ctx    context.Context
	done   chan struct{}
	err    error // Published before done closes.
}

func serveLateAsyncPipe(t *testing.T, server *Server, local, remote net.Conn) *lateAsyncPeer {
	t.Helper()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(errors.Join(err, local.Close(), remote.Close()))
	}
	ctx, stop := context.WithTimeout(t.Context(), 3*time.Second)
	// Keep ServeConn alive independently of test cleanup cancellation.
	serverCtx := context.WithoutCancel(t.Context())
	peer := &lateAsyncPeer{client: client, ctx: ctx, done: make(chan struct{})}
	go func() {
		peer.err = server.ServeConn(serverCtx, local)
		close(peer.done)
	}()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-peer.done:
			if peer.err != nil {
				t.Logf("ServeConn: %v", peer.err)
			}
		case <-time.After(3 * time.Second):
			t.Error("ServeConn did not drain late work")
		}
		stop()
		shutdownCtx, shutdownCancel := context.WithTimeout(serverCtx, 3*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Error(err)
		}
	})
	return peer
}

func waitAsyncSignal(ctx context.Context, t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitAsyncResult(ctx context.Context, t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

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

func testLateAsyncShutdown(t *testing.T, deadline bool) {
	options := testOptions(t)
	logs := make(asyncReplyErrors, 4)
	options.Logger = slog.New(logs)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := newAsyncGate()
	canceled := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ wire.Message) (reply, error) {
		<-ctx.Done()
		close(canceled)
		<-release.done
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
	shutdownCtx := context.WithoutCancel(peer.ctx)
	if deadline {
		var stop context.CancelFunc
		shutdownCtx, stop = context.WithTimeout(peer.ctx, 50*time.Millisecond)
		defer stop()
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- server.Shutdown(shutdownCtx) }()
	waitAsyncSignal(peer.ctx, t, canceled)
	waitAsyncSignal(peer.ctx, t, conn.closed)
	if _, err := peer.client.Receive(peer.ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("shutdown leaked a reply: %v", err)
	}
	writes := conn.writes.Load()
	if deadline {
		if err := waitAsyncResult(peer.ctx, t, shutdown); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown ignored its caller's bound: %v", err)
		}
	} else {
		select {
		case err := <-shutdown:
			t.Fatalf("shutdown returned before late work finished: %v", err)
		default:
		}
	}
	release.release()
	waitAsyncSignal(peer.ctx, t, peer.done)
	if !errors.Is(peer.err, context.Canceled) {
		t.Fatalf("shutdown did not cancel the connection: %v", peer.err)
	}
	if deadline {
		if err := server.Shutdown(peer.ctx); err != nil {
			t.Fatalf("shutdown did not finish after its caller expired: %v", err)
		}
	} else if err := waitAsyncResult(peer.ctx, t, shutdown); err != nil {
		t.Fatal(err)
	}
	if conn.writes.Load() != writes || conn.lateWrites.Load() != 0 {
		t.Fatal("late work wrote a reply after shutdown closed the transport")
	}
	assertNoAsyncFailure(t, logs)
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
	server.handlers[wire.Read] = func(ctx context.Context, _ wire.Message) (reply, error) {
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

func assertNoAsyncFailure(t *testing.T, logs asyncReplyErrors) {
	t.Helper()
	select {
	case err := <-logs:
		t.Fatalf("late result reached the stopped sender: %v", err)
	default:
	}
}
