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
		f.Add(seed.stream)
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

type streamSeed struct {
	name        string
	stream      []byte
	wantReplies int
}

func streamSeeds(t testing.TB) []streamSeed {
	t.Helper()
	negotiate := streamFrame(t, negotiateMessage(t, 16))
	first := streamFrame(t, echo(t, 1))
	second := streamFrame(t, echo(t, 2))
	compound := streamFrame(t, echo(t, 1), echo(t, 2))
	return []streamSeed{
		{name: "negotiate", stream: negotiate, wantReplies: 1},
		{name: "echo stream", stream: bytes.Join([][]byte{negotiate, first, second}, nil), wantReplies: 3},
		{name: "echo compound", stream: bytes.Join([][]byte{negotiate, compound}, nil), wantReplies: 3},
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
	// The test context is canceled before cleanup, but the peer-close wait must not be.
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if err := ctx.Err(); err != nil {
			t.Errorf("server context canceled before peer-close wait: %v", err)
		}
		select {
		case err := <-done:
			// Protocol rejection is an expected outcome for mutated inputs.
			if err != nil {
				t.Logf("ServeConn: %v", err)
			}
		case <-time.After(streamBound):
			t.Error("ServeConn did not stop within the bound")
		}
		cancel()
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), streamBound)
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
// One deadline bounds the whole stream, not each frame.
// A complete request must get a framed reply or a peer close. CANCEL has no
// reply. An incomplete final frame is followed by a client close, not a read
// deadline: the server cannot answer bytes it has not received.
func feedStream(conn net.Conn, stream []byte, bound time.Duration) (replies []wire.Message, closed bool, err error) {
	defer func() { err = errors.Join(err, conn.Close()) }()
	deadlineErr := conn.SetDeadline(time.Now().Add(bound))
	if streamPeerClosed(deadlineErr) {
		return nil, true, nil
	}
	if deadlineErr != nil {
		return nil, false, deadlineErr
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
		reader := &streamReplyReader{Reader: conn}
		payload, readErr = readFrame(reader, max(smb.MaxTransactSize, smb.MaxReadSize, smb.MaxWriteSize)+smb.CreditUnit)
		if reader.started && streamPeerClosed(readErr) {
			readErr = io.ErrUnexpectedEOF
		}
		if readErr != nil {
			readErr = fmt.Errorf("read reply: %w", readErr)
			// Unblock the writer before waiting for its result.
			if err := conn.Close(); err != nil {
				return nil, false, errors.Join(readErr, err, <-written)
			}
		}
	}
	writeErr := <-written
	if readErr != nil && !streamPeerClosed(readErr) {
		return nil, false, readErr
	}
	if writeErr != nil && !streamPeerClosed(writeErr) {
		return nil, false, fmt.Errorf("write request: %w", writeErr)
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

type streamReplyReader struct {
	io.Reader
	started bool
}

func (reader *streamReplyReader) Read(buffer []byte) (int, error) {
	n, err := reader.Reader.Read(buffer)
	reader.started = reader.started || n > 0
	return n, err
}

func streamPeerClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed)
}
