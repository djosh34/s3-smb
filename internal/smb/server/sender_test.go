package server

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb/smbtest"
)

type controlledWrite struct {
	release chan error
}

type controlledConn struct {
	net.Conn
	writes chan controlledWrite
	count  atomic.Int32
}

func (conn *controlledConn) Write(data []byte) (int, error) {
	conn.count.Add(1)
	call := controlledWrite{release: make(chan error, 1)}
	conn.writes <- call
	err := <-call.release
	if err == nil {
		return conn.Conn.Write(data)
	}
	n, writeErr := conn.Conn.Write(data[:5])
	return n, errors.Join(err, writeErr)
}

func senderPipe(t *testing.T) (*sender, *controlledConn, *smbtest.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	local, remote := net.Pipe()
	conn := &controlledConn{Conn: local, writes: make(chan controlledWrite, 3)}
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	sender := newSender(conn)
	go sender.run(ctx, conn.Close)
	t.Cleanup(func() {
		cancel()
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-sender.done:
		case <-time.After(3 * time.Second):
			t.Error("sender did not stop")
		}
	})
	return sender, conn, client, ctx
}

func waitWrite(ctx context.Context, t *testing.T, conn *controlledConn) controlledWrite {
	t.Helper()
	select {
	case call := <-conn.writes:
		return call
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return controlledWrite{}
	}
}

func noCompletion(t *testing.T, completion <-chan error) {
	t.Helper()
	select {
	case err := <-completion:
		t.Fatalf("premature sender completion: %v", err)
	default:
	}
}

// Regression for #129: a producer cannot observe another frame's completion.
func TestEachProducerReceivesOwnCompletion(t *testing.T) {
	sender, conn, client, ctx := senderPipe(t)
	first := sender.enqueue([]byte("first"))
	firstWrite := waitWrite(ctx, t, conn)
	second := sender.enqueue([]byte("second"))
	noCompletion(t, first)
	noCompletion(t, second)
	firstWrite.release <- nil
	if payload, err := client.ReceiveRaw(ctx); err != nil || string(payload) != "first" {
		t.Fatalf("first frame %q: %v", payload, err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	secondWrite := waitWrite(ctx, t, conn)
	noCompletion(t, second)
	secondWrite.release <- nil
	if payload, err := client.ReceiveRaw(ctx); err != nil || string(payload) != "second" {
		t.Fatalf("second frame %q: %v", payload, err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

// Regression for #130: even a recoverable error ends the outgoing stream.
func TestPartialWriteStopsSenderAndFailsQueuedProducers(t *testing.T) {
	sender, conn, client, ctx := senderPipe(t)
	first := sender.enqueue([]byte("first"))
	call := waitWrite(ctx, t, conn)
	queued := sender.enqueue([]byte("queued"))
	writeErr := errors.New("controlled partial write")
	call.release <- writeErr
	if _, err := client.ReceiveRaw(ctx); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial frame: %v", err)
	}
	for _, completion := range []<-chan error{first, queued, sender.enqueue([]byte("late"))} {
		if err := <-completion; !errors.Is(err, writeErr) {
			t.Fatalf("producer lost write error: %v", err)
		}
	}
	<-sender.done
	if conn.count.Load() != 1 {
		t.Fatalf("sender performed %d writes after failure", conn.count.Load())
	}
}
