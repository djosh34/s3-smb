package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func streamAsyncReply(t *testing.T, id uint64, status smb.Status) wire.Message {
	t.Helper()
	body, err := wire.EncodeErrorResponse(wire.ErrorResponse{})
	if err != nil {
		t.Fatal(err)
	}
	credit := uint16(0)
	if status == smb.StatusPending {
		credit = 3
	}
	return wire.Message{Header: wire.Header{Command: wire.Read, MessageID: id, SessionID: 77, AsyncID: id + 10, Flags: wire.FlagResponse | wire.FlagAsync, Status: status, Credit: credit}, Body: body}
}

func TestStreamReadsInterimAndFinalBeforeNextRequest(t *testing.T) {
	for _, status := range []smb.Status{smb.StatusSuccess, smb.StatusIODeviceError} {
		t.Run(fmt.Sprintf("status_%08x", status), func(t *testing.T) {
			request := echo(t, 1)
			request.Header.Command = wire.Read
			interim := streamAsyncReply(t, 1, smb.StatusPending)
			final := streamAsyncReply(t, 1, status)
			last := echo(t, 2)
			last.Header.Flags = wire.FlagResponse
			conn := streamPeer(t, func(peer net.Conn) error {
				if _, err := readFrame(peer, smb.CreditUnit); err != nil {
					return err
				}
				for _, reply := range []wire.Message{interim, final} {
					if _, err := io.Copy(peer, bytes.NewReader(streamFrame(t, reply))); err != nil {
						return err
					}
				}
				if _, err := readFrame(peer, smb.CreditUnit); err != nil {
					return err
				}
				_, err := io.Copy(peer, bytes.NewReader(streamFrame(t, last)))
				return err
			})
			stream := append(streamFrame(t, request), streamFrame(t, echo(t, 2))...)
			replies, closed, err := feedStream(conn, stream, streamBound)
			if err != nil || closed || len(replies) != 3 || replies[1].Header.Status != status || replies[2].Header.MessageID != 2 {
				t.Fatalf("pending stream: %+v, closed %v, error %v", replies, closed, err)
			}
		})
	}
}

func TestStreamCountsMixedCompoundReplies(t *testing.T) {
	prefix := echo(t, 1)
	prefix.Header.Flags = wire.FlagResponse
	suffix := echo(t, 4)
	suffix.Header.Flags = wire.FlagResponse
	replies := [][]wire.Message{
		{prefix},
		{streamAsyncReply(t, 2, smb.StatusPending)},
		{streamAsyncReply(t, 3, smb.StatusPending)},
		{suffix},
		{streamAsyncReply(t, 3, smb.StatusIODeviceError), streamAsyncReply(t, 2, smb.StatusSuccess)},
	}
	conn := streamPeer(t, func(peer net.Conn) error {
		if _, err := readFrame(peer, smb.CreditUnit); err != nil {
			return err
		}
		for _, messages := range replies {
			if _, err := io.Copy(peer, bytes.NewReader(streamFrame(t, messages...))); err != nil {
				return err
			}
		}
		return nil
	})
	stream := streamFrame(t, echo(t, 1), echo(t, 2), echo(t, 3), echo(t, 4))
	got, closed, err := feedStream(conn, stream, streamBound)
	if err != nil || closed || len(got) != 6 {
		t.Fatalf("mixed compound: %d replies, closed %v, error %v", len(got), closed, err)
	}
}

func TestStreamRequiresFinalAfterPending(t *testing.T) {
	conn := streamPeer(t, func(peer net.Conn) error {
		if _, err := readFrame(peer, smb.CreditUnit); err != nil {
			return err
		}
		_, err := io.Copy(peer, bytes.NewReader(streamFrame(t, streamAsyncReply(t, 1, smb.StatusPending))))
		return err
	})
	_, closed, err := feedStream(conn, streamFrame(t, echo(t, 1)), streamBound)
	if !errors.Is(err, io.ErrUnexpectedEOF) || closed {
		t.Fatalf("missing final: closed %v, error %v", closed, err)
	}
}

func TestStreamRejectsChangedAsyncIdentity(t *testing.T) {
	for _, field := range []string{"async", "session", "message", "flag", "credits"} {
		t.Run(field, func(t *testing.T) {
			final := streamAsyncReply(t, 1, smb.StatusIODeviceError)
			switch field {
			case "async":
				final.Header.AsyncID++
			case "session":
				final.Header.SessionID++
			case "message":
				final.Header.MessageID++
			case "flag":
				final.Header.Flags &^= wire.FlagAsync
				final.Header.AsyncID = 0
			case "credits":
				final.Header.Credit = 1
			}
			conn := streamPeer(t, func(peer net.Conn) error {
				if _, err := readFrame(peer, smb.CreditUnit); err != nil {
					return err
				}
				for _, reply := range []wire.Message{streamAsyncReply(t, 1, smb.StatusPending), final} {
					if _, err := io.Copy(peer, bytes.NewReader(streamFrame(t, reply))); err != nil {
						return err
					}
				}
				return nil
			})
			_, closed, err := feedStream(conn, streamFrame(t, echo(t, 1)), streamBound)
			if err == nil || closed {
				t.Fatalf("changed identity accepted: closed %v, error %v", closed, err)
			}
		})
	}
}
