package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestServerStreamSeeds(t *testing.T) {
	storage := fuzzStorage(t)
	root, err := storage.Lookup(t.Context(), "")
	if err != nil || !root.Exists || root.Attr.Kind != smb.KindDirectory {
		t.Fatalf("real adapter root: %+v, %v", root, err)
	}
	for _, seed := range streamSeeds(t) {
		t.Run(seed.name, func(t *testing.T) {
			options := testOptions(t)
			options.Storage = storage
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			replies, closed := runServerStream(t, server, seed.stream)
			if closed || len(replies) != seed.wantReplies {
				t.Fatalf("seed replies: %d, want: %d, closed: %v", len(replies), seed.wantReplies, closed)
			}
			for id, message := range replies {
				if message.Header.Status != smb.StatusSuccess || message.Header.MessageID != uint64(id) {
					t.Fatalf("reply %d: %+v", id, message.Header)
				}
				if id > 0 {
					if _, err := wire.DecodeEchoResponse(message); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestServerStreamCloseAndIncompleteInput(t *testing.T) {
	storage := fuzzStorage(t)
	for _, stream := range [][]byte{
		nil,
		{0, 0},
		{0, 0, 0, 68, 0xfe, 'S', 'M', 'B'},
		bytes.Join([][]byte{streamFrame(t, negotiateMessage(t, 16)), make([]byte, 4)}, nil),
	} {
		options := testOptions(t)
		options.Storage = storage
		server, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		runServerStream(t, server, stream)
	}
}

func TestStreamDeadlineIsNotPeerClose(t *testing.T) {
	conn := streamPeer(t, func(peer net.Conn) error {
		_, err := io.Copy(io.Discard, peer)
		return err
	})
	_, closed, err := feedStream(conn, streamFrame(t, echo(t, 1)), 50*time.Millisecond)
	if !errors.Is(err, os.ErrDeadlineExceeded) || closed {
		t.Fatalf("silent peer: closed %v, error %v", closed, err)
	}
}

func TestStreamChecksEveryFrame(t *testing.T) {
	response := echo(t, 1)
	response.Header.Flags = wire.FlagResponse
	reply := streamFrame(t, response)
	conn := streamPeer(t, func(peer net.Conn) error {
		if _, err := readFrame(peer, smb.CreditUnit); err != nil {
			return err
		}
		if _, err := io.Copy(peer, bytes.NewReader(reply)); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, peer)
		return err
	})
	stream := bytes.Join([][]byte{streamFrame(t, echo(t, 1)), streamFrame(t, echo(t, 2))}, nil)
	replies, closed, err := feedStream(conn, stream, 50*time.Millisecond)
	if len(replies) != 1 || closed || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("second unanswered frame: replies %d, closed %v, error %v", len(replies), closed, err)
	}
}

func TestStreamAcceptsPeerClose(t *testing.T) {
	conn := streamPeer(t, func(peer net.Conn) error { return peer.Close() })
	_, closed, err := feedStream(conn, streamFrame(t, echo(t, 1)), streamBound)
	if err != nil || !closed {
		t.Fatalf("peer close: closed %v, error %v", closed, err)
	}
}

func TestStreamRejectsTruncatedReply(t *testing.T) {
	response := echo(t, 1)
	response.Header.Flags = wire.FlagResponse
	reply := streamFrame(t, response)
	for _, test := range []struct {
		name   string
		length int
	}{
		{name: "one prefix byte", length: 1},
		{name: "two prefix bytes", length: 2},
		{name: "three prefix bytes", length: 3},
		{name: "prefix only", length: 4},
		{name: "body", length: len(reply) - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := streamPeer(t, func(peer net.Conn) error {
				if _, err := readFrame(peer, smb.CreditUnit); err != nil {
					return err
				}
				_, err := io.Copy(peer, bytes.NewReader(reply[:test.length]))
				return err
			})
			_, closed, err := feedStream(conn, streamFrame(t, echo(t, 1)), streamBound)
			if !errors.Is(err, io.ErrUnexpectedEOF) || closed {
				t.Fatalf("truncated reply: closed %v, error %v", closed, err)
			}
		})
	}
}

func TestStreamAlreadyClosedPeer(t *testing.T) {
	local, remote := net.Pipe()
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	_, closed, err := feedStream(local, streamFrame(t, echo(t, 1)), streamBound)
	if err != nil || !closed {
		t.Fatalf("already closed peer: closed %v, error %v", closed, err)
	}
}

func TestStreamCancelNeedsNoReply(t *testing.T) {
	body, encodeErr := wire.EncodeCancelRequest(wire.EmptyRequest{})
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	conn := streamPeer(t, func(peer net.Conn) error {
		_, err := io.Copy(io.Discard, peer)
		return err
	})
	frame := streamFrame(t, wire.Message{Header: wire.Header{Command: wire.Cancel}, Body: body})
	_, closed, err := feedStream(conn, frame, streamBound)
	if err != nil || closed {
		t.Fatalf("CANCEL: closed %v, error %v", closed, err)
	}
}

func TestStreamReportsReadError(t *testing.T) {
	conn := streamPeer(t, func(peer net.Conn) error {
		if _, err := readFrame(peer, smb.CreditUnit); err != nil {
			return err
		}
		_, err := io.Copy(peer, bytes.NewReader(make([]byte, 4)))
		return err
	})
	_, closed, err := feedStream(conn, streamFrame(t, echo(t, 1)), streamBound)
	if err == nil || closed || !strings.HasPrefix(err.Error(), "read reply: ") {
		t.Fatalf("malformed reply frame: closed %v, error %v", closed, err)
	}
}

func TestStreamRejectsInvalidReply(t *testing.T) {
	request := streamFrame(t, echo(t, 1))
	conn := streamPeer(t, func(peer net.Conn) error {
		if _, err := readFrame(peer, smb.CreditUnit); err != nil {
			return err
		}
		_, err := io.Copy(peer, bytes.NewReader(request))
		return err
	})
	_, closed, err := feedStream(conn, request, streamBound)
	if err == nil || closed {
		t.Fatalf("request sent as reply: closed %v, error %v", closed, err)
	}
}

func streamPeer(t *testing.T, serve func(net.Conn) error) net.Conn {
	t.Helper()
	local, remote := net.Pipe()
	done := make(chan error, 1)
	go func() {
		err := serve(remote)
		done <- errors.Join(err, remote.Close())
	}()
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(streamBound):
			t.Error("test peer did not stop")
		}
	})
	return local
}
