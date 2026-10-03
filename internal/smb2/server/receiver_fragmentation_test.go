// SPDX-License-Identifier: AGPL-3.0-only
package smb2

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"syscall"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

// receiverFragmentConn is an artificial finite reader, NOT a TCP simulator.
// It implements the legal io.Reader short-read contract; cuts are absolute
// stream offsets that a Read cannot cross. Only Read and Close are used by the
// real directTCP/runReciever below; other embedded net.Conn methods are unused.
// eofWithData also exercises the legal (n > 0, io.EOF) final-read convention.
type receiverFragmentConn struct {
	net.Conn
	data        []byte
	cuts        []int
	offset      int
	cut         int
	maxRead     int
	eofWithData bool
}

func (r *receiverFragmentConn) Read(p []byte) (int, error) {
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	end := len(r.data)
	for r.cut < len(r.cuts) && r.cuts[r.cut] <= r.offset {
		r.cut++
	}
	if r.cut < len(r.cuts) {
		end = r.cuts[r.cut]
	}
	if r.maxRead > 0 && len(p) > r.maxRead {
		p = p[:r.maxRead]
	}
	n := copy(p, r.data[r.offset:end])
	r.offset += n
	if r.eofWithData && r.offset == len(r.data) {
		return n, io.EOF
	}
	return n, nil
}

func (*receiverFragmentConn) Close() error { return nil }

func receiverFrame(body []byte) []byte {
	wire := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(wire[:4], uint32(len(body)))
	copy(wire[4:], body)
	return wire
}

func receiverWrite(messageID uint64, dataSize int) []byte {
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i*37 + int(messageID))
	}
	r := &WriteRequest{FileId: &FileId{}, Data: data}
	r.MessageId = messageID
	r.CreditCharge = uint16((dataSize + 65535) / 65536)
	packet := make([]byte, r.Size())
	r.Encode(packet)
	return packet
}

// Observe the production receive path up to its request queue. These are valid
// encoded WRITE records, not an authenticated SMB conversation: no server Run,
// sender, signature verification, file operation or storage backend runs here.
// Compare entire packets without ever printing their contents.
func observeReceiver(t *testing.T, wire net.Conn, want [][]byte) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := &conn{
		t: direct(wire), ctx: ctx, cancel: cancel,
		outstandingRequests: newOutstandingRequests(),
		rdone:               make(chan struct{}, 1), wdone: make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runReciever()
	}()
	t.Cleanup(func() {
		cancel()
		wire.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("receiver did not drain")
		}
	})
	count := 0
	for {
		packet, _, _, err := c.srvRecv()
		if err != nil {
			break
		}
		if count >= len(want) {
			t.Errorf("unexpected request %d (length %d)", count, len(packet))
		} else if !bytes.Equal(packet, want[count]) {
			t.Errorf("request %d differs: got length %d, want %d", count, len(packet), len(want[count]))
		}
		count++
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receiver did not exit after EOF/error")
	}
	if count != len(want) {
		t.Errorf("received %d requests, want %d; final error: %v", count, len(want), c.err)
	}
	return c.err
}

func requireReceiverEOF(t *testing.T, err, want error) {
	t.Helper()
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || transportErr.Err != want {
		t.Fatalf("receiver error = %v (%T), want TransportError{%v}", err, err, want)
	}
}

func TestReceiverFragmentationBaseline(t *testing.T) {
	packets := [][]byte{receiverWrite(1, 13), receiverWrite(2, 257)}
	stream := append(receiverFrame(packets[0]), receiverFrame(packets[1])...)
	for _, maxRead := range []int{1, 2, 3, 4, 7, 64, len(stream)} {
		t.Run(fmt.Sprintf("max-read-%d", maxRead), func(t *testing.T) {
			err := observeReceiver(t, &receiverFragmentConn{data: stream, maxRead: maxRead}, packets)
			requireReceiverEOF(t, err, io.EOF)
		})
	}
}

// Also test directTCP in isolation so a queue/SMB parse failure is distinct
// from a transport read failure. EOF at the next prefix is expected termination.
func observeDirectTCP(t *testing.T, wire net.Conn, want [][]byte) {
	t.Helper()
	tr := direct(wire)
	defer tr.Close()
	for i, packet := range want {
		size, err := tr.ReadSize()
		if err != nil || size != len(packet) {
			t.Fatalf("record %d size=%d error=%v, want %d", i, size, err, len(packet))
		}
		got := make([]byte, size)
		n, err := tr.Read(got)
		if err != nil || n != size || !bytes.Equal(got, packet) {
			t.Fatalf("record %d read=%d/%d error=%v equal=%v", i, n, size, err, bytes.Equal(got, packet))
		}
	}
	if _, err := tr.ReadSize(); err != io.EOF {
		t.Fatalf("final prefix error=%v, want EOF", err)
	}
}

func TestReceiverFragmentationEverySplit(t *testing.T) {
	packets := [][]byte{receiverWrite(1, 13), receiverWrite(2, 257)}
	stream := append(receiverFrame(packets[0]), receiverFrame(packets[1])...)
	for cut := 1; cut < len(stream); cut++ {
		t.Run(fmt.Sprintf("offset-%d", cut), func(t *testing.T) {
			observeDirectTCP(t, &receiverFragmentConn{data: stream, cuts: []int{cut}}, packets)
			err := observeReceiver(t, &receiverFragmentConn{data: stream, cuts: []int{cut}}, packets)
			requireReceiverEOF(t, err, io.EOF)
		})
	}
}

func TestReceiverFragmentationSeededPartitions(t *testing.T) {
	packets := [][]byte{receiverWrite(1, 257), receiverWrite(2, 65536), receiverWrite(3, 1<<20)}
	var stream []byte
	for _, p := range packets {
		stream = append(stream, receiverFrame(p)...)
	}
	for seed := int64(0); seed < 100; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			var cuts []int
			for offset := 1; offset < len(stream); offset += 1 + rng.Intn(65537) {
				cuts = append(cuts, offset)
			}
			observeDirectTCP(t, &receiverFragmentConn{data: stream, cuts: cuts}, packets)
			err := observeReceiver(t, &receiverFragmentConn{data: stream, cuts: cuts}, packets)
			requireReceiverEOF(t, err, io.EOF)
		})
	}
}

func TestReceiverFragmentationDataAndEOF(t *testing.T) {
	packets := [][]byte{receiverWrite(1, 13), receiverWrite(2, 257)}
	stream := append(receiverFrame(packets[0]), receiverFrame(packets[1])...)
	for _, maxRead := range []int{1, 3, len(stream)} {
		t.Run(fmt.Sprintf("max-read-%d", maxRead), func(t *testing.T) {
			observeDirectTCP(t, &receiverFragmentConn{data: stream, maxRead: maxRead, eofWithData: true}, packets)
			err := observeReceiver(t, &receiverFragmentConn{data: stream, maxRead: maxRead, eofWithData: true}, packets)
			requireReceiverEOF(t, err, io.EOF)
		})
	}
}

func TestReceiverFragmentationLargeWrites(t *testing.T) {
	// The 24-bit maximum is transport-valid, not a claim that an SMB session
	// negotiates a 16 MiB maximum WRITE. No application state machine runs here.
	for _, size := range []int{65535, 65536, 65537, 1 << 20, 4 << 20, 8 << 20, maxDirectTCPSize - 1, maxDirectTCPSize} {
		t.Run(fmt.Sprintf("body-%d", size), func(t *testing.T) {
			packets := [][]byte{receiverWrite(1, size-112), receiverWrite(2, 13)}
			stream := append(receiverFrame(packets[0]), receiverFrame(packets[1])...)
			cuts := []int{1, 2, 3, 4, 65535, 65536, size + 3, size + 4, size + 5, size + 7}
			observeDirectTCP(t, &receiverFragmentConn{data: stream, cuts: cuts, maxRead: 16381}, packets)
			err := observeReceiver(t, &receiverFragmentConn{data: stream, cuts: cuts, maxRead: 16381}, packets)
			requireReceiverEOF(t, err, io.EOF)
		})
	}
}

func TestReceiverFragmentationTruncatedClose(t *testing.T) {
	prefix, packet := receiverWrite(1, 13), receiverWrite(2, 257)
	frame := receiverFrame(packet)
	for cut := 0; cut <= len(frame); cut++ {
		t.Run(fmt.Sprintf("closed-at-%d", cut), func(t *testing.T) {
			stream := append(receiverFrame(prefix), frame[:cut]...)
			wantPackets := [][]byte{prefix}
			wantErr := error(io.ErrUnexpectedEOF)
			// EOF with zero bytes read at either the next prefix or body is
			// distinct from a partially read prefix/body (UnexpectedEOF).
			if cut == 0 || cut == 4 || cut == len(frame) {
				wantErr = io.EOF
			}
			if cut == len(frame) {
				wantPackets = append(wantPackets, packet)
			}
			err := observeReceiver(t, &receiverFragmentConn{data: stream, maxRead: 3}, wantPackets)
			requireReceiverEOF(t, err, wantErr)
		})
	}
}

// Real Linux loopback sockets; writes intentionally split or coalesce records.
// TCP may merge/split these writes again, so this is not a claim of controlled
// kernel packetization. No reader adapter is interposed on the server socket.
func receiverLoopback(t *testing.T, stream []byte, chunks []int) net.Conn {
	t.Helper()
	return receiverLoopbackClose(t, stream, chunks, false)
}

func receiverLoopbackClose(t *testing.T, stream []byte, chunks []int, reset bool) net.Conn {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	peer, err := listener.AcceptTCP()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	if err = client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err = peer.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if reset {
		if err = client.SetLinger(0); err != nil {
			t.Fatal(err)
		}
	}
	written := make(chan error, 1)
	go func() {
		for offset, i := 0, 0; offset < len(stream); i++ {
			end := len(stream)
			if len(chunks) > 0 {
				end = min(end, offset+chunks[i%len(chunks)])
			}
			n, err := client.Write(stream[offset:end])
			if err == nil && n != end-offset {
				err = io.ErrShortWrite
			}
			if err != nil {
				written <- err
				return
			}
			offset += n
		}
		if reset {
			written <- client.Close()
		} else {
			written <- client.CloseWrite()
		}
	}()
	t.Cleanup(func() {
		client.Close()
		peer.Close()
		select {
		case err := <-written:
			if err != nil {
				t.Errorf("loopback writer: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("loopback writer did not drain")
		}
	})
	return peer
}

func TestReceiverFragmentationLoopback(t *testing.T) {
	var packets [][]byte
	var stream []byte
	for i := 0; i < 32; i++ {
		p := receiverWrite(uint64(i+1), []int{13, 65536, 1 << 20, 257}[i%4])
		packets = append(packets, p)
		stream = append(stream, receiverFrame(p)...)
	}
	for name, chunks := range map[string][]int{
		"coalesced": nil,
		"split":     {1, 2, 3, 7, 65535, 4, 65536, 4093},
	} {
		t.Run(name, func(t *testing.T) {
			observeDirectTCP(t, receiverLoopback(t, stream, chunks), packets)
			err := observeReceiver(t, receiverLoopback(t, stream, chunks), packets)
			requireReceiverEOF(t, err, io.EOF)
		})
	}
}

func TestReceiverFragmentationLoopbackTruncatedClose(t *testing.T) {
	prefix, packet := receiverWrite(1, 13), receiverWrite(2, 1<<20)
	frame := receiverFrame(packet)
	for _, cut := range []int{0, 1, 2, 3, 4, 5, 6, 7, 63, 64, 65535, 65536, len(frame) - 1, len(frame)} {
		t.Run(fmt.Sprintf("closed-at-%d", cut), func(t *testing.T) {
			stream := append(receiverFrame(prefix), frame[:cut]...)
			wantPackets := [][]byte{prefix}
			wantErr := error(io.ErrUnexpectedEOF)
			if cut == 0 || cut == 4 || cut == len(frame) {
				wantErr = io.EOF
			}
			if cut == len(frame) {
				wantPackets = append(wantPackets, packet)
			}
			err := observeReceiver(t, receiverLoopback(t, stream, []int{1, 3, 65537}), wantPackets)
			requireReceiverEOF(t, err, wantErr)
		})
	}
}

func TestReceiverFragmentationLoopbackReset(t *testing.T) {
	frame := receiverFrame(receiverWrite(1, 1<<20))
	for _, cut := range []int{1, 2, 3, 4, 5, 6, 7, 64, 65535, 65536, len(frame) - 1} {
		t.Run(fmt.Sprintf("reset-after-write-%d", cut), func(t *testing.T) {
			// A TCP RST need not preserve all queued bytes. Only a prefix
			// of one record is sent, so regardless of how many bytes the
			// kernel delivers, no whole request may reach the queue.
			wire := receiverLoopbackClose(t, frame[:cut], nil, true)
			err := observeReceiver(t, wire, nil)
			var transportErr *TransportError
			if !errors.As(err, &transportErr) || !errors.Is(transportErr.Err, syscall.ECONNRESET) {
				t.Fatalf("error = %v (%T), want transport connection reset", err, err)
			}
		})
	}
}

// Deliberately malformed controls establish exact error identities. They are
// NOT replays, legal keepalives, or explanations of issue64's historical bytes.
func TestReceiverFragmentationErrorControls(t *testing.T) {
	prefix := receiverWrite(1, 13)
	for size := 0; size < 4; size++ {
		t.Run(fmt.Sprintf("declared-short-body-%d", size), func(t *testing.T) {
			stream := append(receiverFrame(prefix), receiverFrame(make([]byte, size))...)
			err := observeReceiver(t, &receiverFragmentConn{data: stream, maxRead: 1}, [][]byte{prefix})
			var invalid *InvalidRequestError
			if !errors.As(err, &invalid) || err.Error() != "invalid request error: short client packet header" {
				t.Fatalf("error = %v (%T), want exact short client packet header", err, err)
			}
		})
	}
	t.Run("nonzero-type", func(t *testing.T) {
		stream := append(receiverFrame(prefix), 0x85, 0, 0, 0)
		err := observeReceiver(t, &receiverFragmentConn{data: stream, maxRead: 1}, [][]byte{prefix})
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || err.Error() != "connection error: invalid transport format" {
			t.Fatalf("error = %v (%T), want exact invalid transport format", err, err)
		}
	})
}
