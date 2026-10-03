package smb2

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

// streamConn gives the real receiver a deterministic TCP byte stream with
// controllable read fragmentation; it never changes the bytes on the wire.
type streamConn struct{ io.Reader }

func (*streamConn) Write(p []byte) (int, error)      { return len(p), nil }
func (*streamConn) Close() error                     { return nil }
func (*streamConn) LocalAddr() net.Addr              { return nil }
func (*streamConn) RemoteAddr() net.Addr             { return nil }
func (*streamConn) SetDeadline(time.Time) error      { return nil }
func (*streamConn) SetReadDeadline(time.Time) error  { return nil }
func (*streamConn) SetWriteDeadline(time.Time) error { return nil }

type chunkReader struct {
	io.Reader
	max int
}

func (r chunkReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.Reader.Read(p)
}
func streamFrame(p []byte) []byte {
	b := make([]byte, 4+len(p))
	be.PutUint32(b, uint32(len(p)))
	copy(b[4:], p)
	return b
}

func runStreamReceiver(t *testing.T, wire []byte, chunk int) ([][]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := &conn{t: direct(&streamConn{chunkReader{bytes.NewReader(wire), chunk}}), ctx: ctx, cancel: cancel,
		outstandingRequests: newOutstandingRequests(), rdone: make(chan struct{}, 1), wdone: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() { defer close(done); c.runReciever() }()
	var packets [][]byte
	for {
		p, _, _, err := c.srvRecv()
		if err != nil {
			break
		}
		packets = append(packets, append([]byte(nil), p...))
	}
	<-done
	return packets, c.err
}

func TestTransportStreamExactSymptoms(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
		want string
	}{
		{"zero", []byte{0, 0, 0, 0}, "invalid request error: short client packet header"},
		{"three", []byte{0, 0, 0, 3, 0, 0, 0}, "invalid request error: short client packet header"},
		{"nonzero_type", []byte{0x85, 0, 0, 0}, "connection error: invalid transport format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runStreamReceiver(t, tc.wire, 1)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			t.Log(err)
		})
	}
}

func TestTransportStreamValidSplitCoalesced(t *testing.T) {
	var wire []byte
	var want [][]byte
	for i := 0; i < 100; i++ {
		r := &EchoResponse{}
		r.MessageId = uint64(i)
		p := requestBytes(r)
		wire = append(wire, streamFrame(p)...)
		want = append(want, p)
	}
	for _, chunk := range []int{1, 2, 3, 4, 7, 68, 4096, len(wire)} {
		t.Run(string(rune('A'+chunk%26)), func(t *testing.T) {
			got, err := runStreamReceiver(t, wire, chunk)
			if err == nil || err.Error() != "connection error: EOF" {
				t.Fatalf("chunk %d: %v", chunk, err)
			}
			if len(got) != len(want) {
				t.Fatalf("chunk %d: got %d packets", chunk, len(got))
			}
			for i := range got {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("chunk %d packet %d changed", chunk, i)
				}
			}
		})
	}
}
