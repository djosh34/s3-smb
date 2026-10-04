package server

import (
	"errors"
	"io"
	"testing"
	"time"
)

// Regression for #129: a producer cannot observe another frame's completion.
func TestEachProducerReceivesOwnCompletion(t *testing.T) {
	sender, conn, client, ctx := senderPipe(t)
	first := sender.enqueue([]byte("first"))
	firstWrite := waitWrite(ctx, t, conn)
	second := make(chan error, 1)
	queued := make(chan struct{})
	go func() {
		completion := sender.enqueue([]byte("second"))
		close(queued)
		second <- <-completion
	}()
	<-queued
	noCompletion(t, first)
	noCompletion(t, second)
	firstWrite.release <- nil
	if payload, err := client.ReceiveRaw(ctx); err != nil || string(payload) != "first" {
		t.Fatalf("first frame %q: %v", payload, err)
	}
	secondWrite := waitWrite(ctx, t, conn)
	// The second producer is already waiting while the first result remains
	// unread. A shared completion channel would deliver that result to it.
	select {
	case err := <-second:
		t.Fatalf("second producer stole first completion: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
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
