package smbtest_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func fakePeer(t *testing.T, serve func(net.Conn) error) *smbtest.Client {
	t.Helper()
	conn, peer := net.Pipe()
	client, err := smbtest.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(peer) }()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		if err := peer.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Errorf("fake peer: %v", err)
		}
	})
	return client
}

func readFrame(conn net.Conn) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return nil, err
	}
	if prefix[0] != 0 {
		return nil, errors.New("invalid frame type")
	}
	length := int(prefix[1])<<16 | int(prefix[2])<<8 | int(prefix[3])
	payload := make([]byte, length)
	_, err := io.ReadFull(conn, payload)
	return payload, err
}

func writePayload(conn net.Conn, payload []byte) error {
	if len(payload) > 0xffffff {
		return errors.New("test payload too long")
	}
	length := uint32(len(payload))
	frame := []byte{0, byte(length >> 16), byte(length >> 8 & 0xff), byte(length & 0xff)}
	frame = append(frame, payload...)
	for len(frame) > 0 {
		n, err := conn.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		frame = frame[n:]
	}
	return nil
}

func writeMessages(conn net.Conn, messages ...wire.Message) error {
	payload, err := wire.Join(messages)
	if err != nil {
		return err
	}
	return writePayload(conn, payload)
}

func TestSendPreservesHeadersAndCredits(t *testing.T) {
	messages := []wire.Message{
		{Header: wire.Header{Command: wire.Echo, MessageID: 31, SessionID: 72, TreeID: 13, ProcessID: 42, ChannelSequence: 9, Flags: wire.FlagSigned | wire.FlagReplay, CreditCharge: 17, Credit: 0, NextCommand: 128, Signature: [16]byte{1, 2, 3}}, Body: []byte{9, 8, 7}, Raw: []byte("ignored")},
		{Header: wire.Header{Command: wire.Write, MessageID: 99, SessionID: ^uint64(0), TreeID: ^uint32(0), Flags: wire.FlagRelated, CreditCharge: 0, Credit: 65535}, Body: []byte{1, 0, 0, 0}},
	}
	original := append([]wire.Message(nil), messages...)
	client := fakePeer(t, func(peer net.Conn) error {
		payload, err := readFrame(peer)
		if err != nil {
			return err
		}
		if len(payload) != 140 {
			return fmt.Errorf("payload length = %d, want 140", len(payload))
		}
		decoded, err := wire.Split(payload)
		if err != nil {
			return err
		}
		if len(decoded) != len(messages) {
			return errors.New("wrong compound member count")
		}
		for i, message := range decoded {
			want := messages[i].Header
			want.NextCommand = 0
			if i == 0 {
				want.NextCommand = 72
			}
			if message.Header != want {
				return fmt.Errorf("member %d header = %+v, want %+v", i, message.Header, want)
			}
			if !bytes.HasPrefix(message.Body, messages[i].Body) {
				return fmt.Errorf("member %d body changed", i)
			}
		}
		if !bytes.Equal(payload[67:72], make([]byte, 5)) {
			return errors.New("compound padding is not zero")
		}
		return nil
	})
	if err := client.Send(t.Context(), messages); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(messages, original) {
		t.Fatal("Send changed supplied messages")
	}
}

func TestRawBytesStayUnchanged(t *testing.T) {
	// The claimed length, frame type and SMB magic are deliberately wrong.
	framed := []byte{0x81, 0xff, 0xff, 0xff, 0xfe, 'X', 'M', 'B', 0}
	payloads := [][]byte{{0xfe, 'S', 'M'}, {}, {1, 2, 3, 4}}
	client := fakePeer(t, func(peer net.Conn) error {
		received := make([]byte, len(framed))
		if _, err := io.ReadFull(peer, received); err != nil {
			return err
		}
		if !bytes.Equal(received, framed) {
			return errors.New("raw send changed bytes")
		}
		for _, payload := range payloads {
			if err := writePayload(peer, payload); err != nil {
				return err
			}
		}
		return nil
	})
	if err := client.SendRaw(t.Context(), framed); err != nil {
		t.Fatal(err)
	}
	for _, want := range payloads {
		got, err := client.ReceiveRaw(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("raw payload = %x, want %x", got, want)
		}
	}
}

func response(messageID, asyncID uint64, status smb.Status, credit uint16) wire.Message {
	flags := wire.FlagResponse
	if asyncID != 0 {
		flags |= wire.FlagAsync
	}
	return wire.Message{Header: wire.Header{Command: wire.Read, Flags: flags, MessageID: messageID, AsyncID: asyncID, SessionID: 45, Status: status, Credit: credit}, Body: []byte{9, 0, 0, 0, 0, 0, 0, 0}}
}

func TestPendingAndFinalReplies(t *testing.T) {
	frames := [][]wire.Message{
		{response(10, 110, smb.StatusPending, 5), response(20, 120, smb.StatusPending, 7)},
		{response(30, 0, smb.StatusSuccess, 2)},
		{response(20, 120, smb.StatusAccessDenied, 0)},
		{response(10, 110, smb.StatusSuccess, 0)},
	}
	client := fakePeer(t, func(peer net.Conn) error {
		for _, messages := range frames {
			if err := writeMessages(peer, messages...); err != nil {
				return err
			}
		}
		return nil
	})
	for _, want := range frames {
		reply, err := client.Receive(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := wire.Join(want)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(reply.Raw, encoded) {
			t.Fatal("received frame changed")
		}
		decoded, err := wire.Split(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(reply.Messages, decoded) {
			t.Fatalf("received messages = %+v, want %+v", reply.Messages, decoded)
		}
	}
}

func TestAsyncIdentityMismatch(t *testing.T) {
	cases := map[string]wire.Message{
		"message ID":         response(11, 110, smb.StatusSuccess, 0),
		"async ID":           response(10, 111, smb.StatusSuccess, 0),
		"missing async flag": response(10, 0, smb.StatusSuccess, 0),
		"duplicate pending":  response(10, 110, smb.StatusPending, 5),
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			client := fakePeer(t, func(peer net.Conn) error {
				return errors.Join(
					writeMessages(peer, response(10, 110, smb.StatusPending, 5)),
					writeMessages(peer, invalid),
					writeMessages(peer, response(10, 110, smb.StatusSuccess, 0)),
				)
			})
			if _, err := client.Receive(t.Context()); err != nil {
				t.Fatal(err)
			}
			reply, err := client.Receive(t.Context())
			if err == nil || len(reply.Messages) != 1 || reply.Messages[0].Header != invalid.Header {
				t.Fatalf("invalid reply = %+v, error = %v", reply, err)
			}
			if _, err := client.Receive(t.Context()); err != nil {
				t.Fatalf("valid final after mismatch: %v", err)
			}
		})
	}
}

func TestInvalidReplies(t *testing.T) {
	for name, message := range map[string]wire.Message{
		"pending without async flag": response(10, 0, smb.StatusPending, 5),
		"final without pending":      response(10, 110, smb.StatusSuccess, 0),
	} {
		t.Run(name, func(t *testing.T) {
			client := fakePeer(t, func(peer net.Conn) error { return writeMessages(peer, message) })
			if _, err := client.Receive(t.Context()); err == nil {
				t.Fatal("invalid reply accepted")
			}
		})
	}
	t.Run("malformed SMB", func(t *testing.T) {
		client := fakePeer(t, func(peer net.Conn) error { return writePayload(peer, []byte{1, 2, 3}) })
		reply, err := client.Receive(t.Context())
		if err == nil || !bytes.Equal(reply.Raw, []byte{1, 2, 3}) {
			t.Fatalf("reply = %+v, error = %v", reply, err)
		}
	})
}

func TestSendAndReceiveConcurrently(t *testing.T) {
	client := fakePeer(t, func(peer net.Conn) error {
		// The peer writes first. A client-wide I/O mutex would deadlock here.
		if err := writeMessages(peer, response(1, 0, smb.StatusSuccess, 5)); err != nil {
			return err
		}
		_, err := readFrame(peer)
		return err
	})
	sent := make(chan error, 1)
	go func() { sent <- client.Send(t.Context(), []wire.Message{{Header: wire.Header{Command: wire.Echo}}}) }()
	if _, err := client.Receive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

func TestNilConnection(t *testing.T) {
	if _, err := smbtest.NewClient(nil); err == nil {
		t.Fatal("nil connection accepted")
	}
}
