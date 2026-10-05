package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCredits(t *testing.T) {
	credits := newCredits()
	consume := func(headers ...wire.Header) error {
		messages := make([]wire.Message, len(headers))
		for index, header := range headers {
			messages[index].Header = header
		}
		return credits.consume(messages)
	}
	// A request for no credits still gets back the one it used.
	negotiate := wire.Header{Command: wire.Negotiate}
	if err := consume(negotiate); err != nil {
		t.Fatal(err)
	}
	if granted := credits.grant(negotiate); granted != 1 {
		t.Fatalf("granted %d", granted)
	}
	// Grants grow the window up to the target and no further.
	if granted := credits.grant(wire.Header{Credit: 1000}); granted != smb.TargetCredits-1 {
		t.Fatalf("granted %d", granted)
	}
	if granted := credits.grant(wire.Header{Credit: 1}); granted != 0 {
		t.Fatalf("granted %d over the target", granted)
	}
	for _, refused := range [][]wire.Header{
		{{MessageID: 0}},                 // used
		{{MessageID: 257}},               // not granted yet
		{{MessageID: 1}, {MessageID: 1}}, // twice in one compound
		{{MessageID: 255, CreditCharge: 3}},
	} {
		if err := consume(refused...); err == nil {
			t.Errorf("consumed %+v", refused)
		}
	}
	// A refused compound uses no ID; a multi-credit request uses a run of them.
	read, flush := wire.Header{MessageID: 1, CreditCharge: 2}, wire.Header{MessageID: 3}
	if err := consume(read, flush); err != nil {
		t.Fatal(err)
	}
	if err := consume(wire.Header{MessageID: 2}); err == nil {
		t.Fatal("consumed the second credit of a READ")
	}
	// Each reply in a compound gives back at least the credits it used.
	if granted := credits.grant(read) + credits.grant(flush); granted != 3 {
		t.Fatalf("granted %d", granted)
	}
	// SESSION_SETUP brings the window up to what a reconnect needs.
	if err := consume(wire.Header{MessageID: 4}, wire.Header{MessageID: 5}, wire.Header{MessageID: 6}, wire.Header{MessageID: 7}, wire.Header{MessageID: 8}); err != nil {
		t.Fatal(err)
	}
	if granted := credits.grant(wire.Header{Command: wire.SessionSetup}); granted != smb.MinReconnectCredits {
		t.Fatalf("granted %d", granted)
	}
}

func TestReadFrame(t *testing.T) {
	prefix := func(kind byte, length uint32) []byte {
		prefix := binary.BigEndian.AppendUint32(nil, length)
		prefix[0] = kind
		return prefix
	}
	payload := bytes.Repeat([]byte{1}, 40)
	got, err := readFrame(bytes.NewReader(append(prefix(0, 40), payload...)), 64)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("frame %x, %v", got, err)
	}
	// Only the prefix is there: a refusal must not wait for the body.
	for _, refused := range [][]byte{prefix(0, 65), prefix(1, 40), prefix(0, 31)} {
		if _, err := readFrame(bytes.NewReader(refused), 64); err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("prefix %x: %v", refused, err)
		}
	}
}

type failingConn struct {
	net.Conn
	writes atomic.Int32
}

var errWrite = errors.New("connection reset")

func (conn *failingConn) Write([]byte) (int, error) {
	conn.writes.Add(1)
	return 0, errWrite
}

// After a failed write the stream may hold part of a frame, so the sender
// writes nothing more, and every frame queued behind it fails too.
func TestSenderStopsAtWriteError(t *testing.T) {
	conn := &failingConn{}
	sender := newSender(conn)
	first, second := sender.enqueue([]byte("first")), sender.enqueue([]byte("second"))
	go sender.run(t.Context(), func() error { return nil })
	for _, result := range []<-chan error{first, second} {
		if err := <-result; !errors.Is(err, errWrite) {
			t.Fatalf("result %v", err)
		}
	}
	<-sender.done
	if err := <-sender.enqueue([]byte("late")); !errors.Is(err, errWrite) {
		t.Fatalf("late result %v", err)
	}
	if conn.writes.Load() != 1 {
		t.Fatalf("%d writes", conn.writes.Load())
	}
}
