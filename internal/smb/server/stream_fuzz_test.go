package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const streamBound = 3 * time.Second

func FuzzServerStream(f *testing.F) {
	for _, seed := range streamSeeds(f) {
		f.Add(seed)
	}
	storage := fuzzStorage(f)
	f.Fuzz(func(t *testing.T, stream []byte) {
		// Bound total work as well as individual frame allocations.
		if len(stream) > 64<<10 {
			t.Skip("stream exceeds corpus limit")
		}
		options := testOptions(t)
		options.Storage = storage
		server, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		runServerStream(t, server, stream)
	})
}

func streamSeeds(t testing.TB) [][]byte {
	t.Helper()
	negotiate := streamFrame(t, negotiateMessage(t, 16))
	first := streamFrame(t, echo(t, 1))
	second := streamFrame(t, echo(t, 2))
	compound := streamFrame(t, echo(t, 1), echo(t, 2))
	return [][]byte{
		negotiate,
		bytes.Join([][]byte{negotiate, first, second}, nil),
		bytes.Join([][]byte{negotiate, compound}, nil),
	}
}

func streamFrame(t testing.TB, messages ...wire.Message) []byte {
	t.Helper()
	payload, err := wire.Join(messages)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 4, 4+len(payload))
	if len(payload) > 0xffffff {
		t.Fatal("seed frame exceeds direct TCP length")
	}
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	return append(frame, payload...)
}

func runServerStream(t *testing.T, server *Server, stream []byte) ([]wire.Message, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	local, remote := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			// Protocol rejection is an expected outcome for mutated inputs.
			if err != nil {
				t.Logf("ServeConn: %v", err)
			}
		case <-time.After(streamBound):
			t.Error("ServeConn did not stop within the bound")
		}
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), streamBound)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Error(err)
		}
	})
	replies, closed, err := feedStream(remote, stream, streamBound)
	if err != nil {
		t.Fatal(err)
	}
	return replies, closed
}

// feedStream preserves frame bytes and connection state between requests.
// A complete request must get a framed reply or a peer close. CANCEL has no
// reply. An incomplete final frame is followed by a client close, not a read
// deadline: the server cannot answer bytes it has not received.
func feedStream(conn net.Conn, stream []byte, bound time.Duration) (replies []wire.Message, closed bool, err error) {
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err := conn.SetDeadline(time.Now().Add(bound)); err != nil {
		return nil, false, err
	}
	for len(stream) > 0 {
		length := len(stream)
		complete := false
		if len(stream) >= 4 {
			length = 4 + int(stream[1])<<16 + int(stream[2])<<8 + int(stream[3])
			complete = length <= len(stream)
			length = min(length, len(stream))
		}
		frame := stream[:length]
		messages, peerClosed, exchangeErr := exchangeStreamFrame(conn, frame, complete && needsStreamReply(frame))
		if exchangeErr != nil {
			return replies, false, exchangeErr
		}
		replies = append(replies, messages...)
		if peerClosed || !complete {
			return replies, peerClosed, nil
		}
		stream = stream[length:]
	}
	return replies, false, nil
}

func needsStreamReply(frame []byte) bool {
	messages, err := wire.Split(frame[4:])
	if err != nil {
		return true
	}
	for _, message := range messages {
		if message.Header.Command != wire.Cancel {
			return true
		}
	}
	return false
}

func exchangeStreamFrame(conn net.Conn, frame []byte, wantReply bool) ([]wire.Message, bool, error) {
	written := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, bytes.NewReader(frame))
		written <- err
	}()
	var payload []byte
	var readErr error
	if wantReply {
		payload, readErr = readFrame(conn, max(smb.MaxTransactSize, smb.MaxReadSize, smb.MaxWriteSize)+smb.CreditUnit)
		if readErr != nil {
			// Unblock the writer before waiting for its result.
			if err := conn.Close(); err != nil {
				return nil, false, errors.Join(readErr, err, <-written)
			}
		}
	}
	writeErr := <-written
	if readErr != nil && !streamPeerClosed(readErr) {
		return nil, false, fmt.Errorf("reply did not arrive within the bound: %w", readErr)
	}
	if writeErr != nil && !streamPeerClosed(writeErr) {
		return nil, false, fmt.Errorf("request did not finish within the bound: %w", writeErr)
	}
	peerClosed := streamPeerClosed(readErr) || streamPeerClosed(writeErr)
	if peerClosed {
		return nil, true, nil
	}
	if !wantReply {
		return nil, false, nil
	}
	messages, err := wire.Split(payload)
	if err != nil {
		return nil, false, fmt.Errorf("invalid reply: %w", err)
	}
	for _, message := range messages {
		if message.Header.Flags&wire.FlagResponse == 0 {
			return nil, false, errors.New("reply lacks response flag")
		}
	}
	return messages, false, nil
}

func streamPeerClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed)
}
