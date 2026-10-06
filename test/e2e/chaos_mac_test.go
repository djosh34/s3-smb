// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	smbproto "github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	fileAllAccess uint32 = 0x001f01ff
	fileShareRead uint32 = 1
	fileOpenIf    uint32 = 3
)

// rawMac is the Mac on the raw SMB test client, for what go-smb2 cannot do:
// durable handles, reconnects and misbehaving connections. One goroutine at a
// time may use it.
type rawMac struct {
	client  *smbtest.Client
	conn    net.Conn
	session smbtest.Session
}

// rawLogin connects to addr and logs in as the Mac, replacing the session
// previous when it is not zero. Cleanup closes the connection.
func (f *fixture) rawLogin(ctx context.Context, addr string, previous uint64) (*rawMac, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	f.t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			f.t.Error(closeErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{
		Share: "TimeMachine", Account: auth.Account{User: "backup", Password: f.password},
		ClientGUID: macGUID, PreviousSessionID: previous,
		Cipher: smbproto.CipherAES128GCM, Signing: smbproto.SigningGMAC,
	})
	if err != nil {
		return nil, err
	}
	return &rawMac{client: client, conn: conn, session: session}, nil
}

// header returns the header of the next request, which costs one credit and
// asks for 32.
func (m *rawMac) header(command wire.Command) wire.Header {
	header := wire.Header{Command: command, MessageID: m.session.NextMessageID, SessionID: m.session.SessionID, TreeID: m.session.TreeID, CreditCharge: 1, Credit: 32}
	m.session.NextMessageID++
	m.session.Credits--
	return header
}

// send sends one request and returns its header.
func (m *rawMac) send(ctx context.Context, command wire.Command, body []byte) (wire.Header, error) {
	header := m.header(command)
	return header, m.client.Send(ctx, []wire.Message{{Header: header, Body: body}})
}

// reply returns the final reply to request, after any interim reply.
func (m *rawMac) reply(ctx context.Context, request wire.Header) (wire.Message, error) {
	for {
		reply, err := m.client.Receive(ctx)
		if err != nil {
			return wire.Message{}, err
		}
		if len(reply.Messages) != 1 || reply.Messages[0].Header.MessageID != request.MessageID {
			return wire.Message{}, fmt.Errorf("reply %+v does not answer message %d", reply.Messages, request.MessageID)
		}
		message := reply.Messages[0]
		m.session.Credits += message.Header.Credit
		if message.Header.Status != smbproto.StatusPending {
			return message, nil
		}
	}
}

// call sends a request that answers with only a status and returns an error
// for any status but success.
func (m *rawMac) call(ctx context.Context, command wire.Command, body []byte, err error) error {
	if err != nil {
		return err
	}
	header, err := m.send(ctx, command, body)
	if err != nil {
		return err
	}
	message, err := m.reply(ctx, header)
	if err == nil && message.Header.Status != smbproto.StatusSuccess {
		err = fmt.Errorf("command %d: status %#x", command, message.Header.Status)
	}
	return err
}

// create returns the reply to a CREATE, or only its status when it failed.
func (m *rawMac) create(ctx context.Context, options smbtest.CreateOptions) (smbtest.CreateResult, smbproto.Status, error) {
	header := m.header(wire.Create)
	if err := m.client.SendCreate(ctx, header, options); err != nil {
		return smbtest.CreateResult{}, 0, err
	}
	message, err := m.reply(ctx, header)
	if err != nil || message.Header.Status != smbproto.StatusSuccess {
		return smbtest.CreateResult{}, message.Header.Status, err
	}
	result, err := smbtest.DecodeCreateReply(message)
	return result, smbproto.StatusSuccess, err
}

func (m *rawMac) write(ctx context.Context, id wire.FileID, offset int, data []byte) error {
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Offset: uint64(offset), Data: data}) //nolint:gosec // Offsets are within a test file.
	return m.call(ctx, wire.Write, body, err)
}

func (m *rawMac) flush(ctx context.Context, id wire.FileID) error {
	body, err := wire.EncodeFlushRequest(wire.FlushRequest{ID: id})
	return m.call(ctx, wire.Flush, body, err)
}

func (m *rawMac) close(ctx context.Context, id wire.FileID) error {
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	return m.call(ctx, wire.Close, body, err)
}

// TestChaosDurableReconnect cuts the Mac's connection at a random point of
// writing a band with a durable handle. Inside the reconnect window, up to 30
// seconds in the gate, the Mac reclaims the open and finishes the band. When
// the outage outlasts the window, the reclaim fails: every acknowledged write
// is still in the file, earlier bands are intact and the next backup writes
// the band. The server side of a lost window is an expired durable handle; the
// macOS client's own 30 second limit needs a Mac.
func TestChaosDurableReconnect(t *testing.T) {
	rng := chaosRand(t)
	rounds, window := 2, 3*time.Second
	if gate() {
		rounds, window = 6, 30*time.Second
	}
	f := newFixture(t)
	d := f.start()
	proxy := f.networkProxy()
	bands := make(map[string][]byte)
	for round := range rounds {
		name := fmt.Sprintf("band-%d", round)
		durableCut(t, f, proxy, rng, bands, name+"-inside", 0, between(rng, 0, window))
		// The Mac asks for a 2 second timeout, so the test need not wait out
		// the default 2 minutes. The server expires opens every second.
		durableCut(t, f, proxy, rng, bands, name+"-beyond", 2000, between(rng, 4*time.Second, 6*time.Second))
	}
	d.alive()
}

// durableCut writes a band through a durable handle with a timeout of timeout
// milliseconds, zero for the default, cuts the connection after a random
// number of pieces and reconnects after outage. It checks and adds to bands,
// the bands written so far.
func durableCut(t *testing.T, f *fixture, proxy *netfault.Proxy, rng *rand.Rand, bands map[string][]byte, name string, timeout uint32, outage time.Duration) {
	t.Helper()
	ctx := t.Context()
	mac, err := f.rawLogin(ctx, proxy.Address(), 0)
	if err != nil {
		t.Fatal(err)
	}
	data := chaosData(rng, 4<<20)
	request := wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: fileShareRead, Disposition: fileOpenIf}
	lease := wire.LeaseContext{Version: 2, Key: [16]byte(chaosData(rng, 16)), State: smbproto.LeaseRead | smbproto.LeaseHandle}
	durable := wire.DurableRequest{CreateGUID: [16]byte(chaosData(rng, 16)), Timeout: timeout}
	created, status, err := mac.create(ctx, smbtest.CreateOptions{Request: request, Lease: &lease, Durable: &durable})
	if err != nil || status != smbproto.StatusSuccess || created.Lease == nil || created.Durable == nil {
		t.Fatalf("durable CREATE: %+v, status %#x, %v", created, status, err)
	}
	cutAt := rng.IntN(len(data) / piece)
	reached, acknowledged := make(chan struct{}), make(chan int, 1)
	reach := sync.OnceFunc(func() { close(reached) })
	go func() {
		// A WRITE that fails before the cut point must not leave the test
		// waiting.
		defer reach()
		written := 0
		for ; written < len(data); written += piece {
			if written == cutAt*piece {
				reach()
			}
			if mac.write(ctx, created.Reply.ID, written, data[written:written+piece]) != nil {
				break
			}
		}
		acknowledged <- written
	}()
	<-reached
	if err = proxy.Drop(); err != nil {
		t.Fatal(err)
	}
	written := <-acknowledged
	if written < cutAt*piece {
		t.Fatalf("WRITE failed at %d before the cut", written)
	}
	time.Sleep(outage)
	proxy.Restore()

	resumed, err := f.rawLogin(ctx, proxy.Address(), mac.session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	reclaim := smbtest.CreateOptions{Request: request, Lease: created.Lease, Reconnect: &wire.DurableReconnect{ID: created.Reply.ID, CreateGUID: durable.CreateGUID}}
	reopened, status, err := resumed.create(ctx, reclaim)
	if err != nil {
		t.Fatal(err)
	}
	share, disconnect := f.chaosShare(f.addr)
	defer disconnect()
	if timeout != 0 {
		if status != smbproto.StatusObjectNameNotFound {
			t.Fatalf("reclaim after the durable timeout: status %#x", status)
		}
		got, err := share.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) < written || len(got) > len(data) || !bytes.Equal(got, data[:len(got)]) {
			t.Fatalf("after the durable timeout the file holds %d bytes, not a prefix of the %d acknowledged bytes", len(got), written)
		}
		verifyFiles(t, share, bands)
		// The next backup writes the band again.
		writeFile(t, share, name, data)
		bands[name] = data
		verifyFiles(t, share, bands)
		return
	}
	if status != smbproto.StatusSuccess {
		t.Fatalf("reclaim after %v: status %#x", outage, status)
	}
	id := reopened.Reply.ID
	for offset := written; offset < len(data); offset += piece {
		if err := resumed.write(ctx, id, offset, data[offset:offset+piece]); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(resumed.flush(ctx, id), resumed.close(ctx, id)); err != nil {
		t.Fatal(err)
	}
	bands[name] = data
	verifyFiles(t, share, bands)
}

// TestChaosBadConnections opens connections that send garbage, stop in the
// middle of a frame, announce a huge frame or never read their replies, while
// the Mac backs up. The backup succeeds and the server keeps running.
func TestChaosBadConnections(t *testing.T) {
	rng := chaosRand(t)
	count := 6
	if gate() {
		count = 24
	}
	f := newFixture(t)
	d := f.start()
	share, disconnect := f.chaosShare(f.addr)
	defer disconnect()
	writeFile(t, share, "readable.bin", chaosData(rng, 4<<20))
	backedUp := make(chan struct{})
	var bad sync.WaitGroup
	// The bad connections end when the backup does, also when it fails.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer func() {
		close(backedUp)
		// After a failed backup, stuck bad connections end too.
		if t.Failed() {
			cancel()
		}
		bad.Wait()
	}()
	for range count {
		kind, garbage, delay := rng.IntN(4), chaosData(rng, 1+rng.IntN(8<<10)), between(rng, 0, time.Second)
		bad.Go(func() {
			time.Sleep(delay)
			if err := misbehave(ctx, f, kind, garbage, backedUp); err != nil {
				t.Error(err)
			}
		})
	}
	files := chaosFiles(rng, "bad-connections", count, 4<<20)
	writeFiles(t, share, files)
	verifyFiles(t, share, files)
	d.alive()
}

// misbehave opens one bad connection of the given kind. Connections that hold
// stay open until backedUp closes.
func misbehave(ctx context.Context, f *fixture, kind int, garbage []byte, backedUp <-chan struct{}) error {
	if kind == 3 {
		return unreadReplies(ctx, f, backedUp)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", f.addr)
	if err != nil {
		return err
	}
	switch kind {
	case 0:
		// Random bytes, read as frames of random length.
		_, err = conn.Write(garbage)
	case 1:
		// A 1 MiB frame that stops after its first bytes.
		_, err = conn.Write(append([]byte{0, 0x10, 0, 0}, garbage[:min(len(garbage), 64)]...))
	case 2:
		// A frame longer than any SMB message.
		_, err = conn.Write(append([]byte{0, 0xff, 0xff, 0xff}, garbage...))
	}
	// The server may close a connection that sent garbage before the write
	// ends.
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		err = nil
	}
	<-backedUp
	return errors.Join(err, conn.Close())
}

// unreadReplies logs in as the Mac on another connection, asks for more READ
// replies than the socket buffers hold and never reads them.
func unreadReplies(ctx context.Context, f *fixture, backedUp <-chan struct{}) error {
	mac, err := f.rawLogin(ctx, f.addr, 0)
	if err != nil {
		return err
	}
	opened, status, err := mac.create(ctx, smbtest.CreateOptions{Request: wire.CreateRequest{Name: "readable.bin", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}})
	if err != nil || status != smbproto.StatusSuccess {
		return fmt.Errorf("open for unread replies: status %#x, %w", status, err)
	}
	// Each ECHO asks for 32 more credits, up to the server's 256.
	for range 8 {
		body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
		if err = mac.call(ctx, wire.Echo, body, err); err != nil {
			return err
		}
	}
	for range mac.session.Credits {
		body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: opened.Reply.ID, Length: piece})
		if err == nil {
			_, err = mac.send(ctx, wire.Read, body)
		}
		if err != nil {
			return err
		}
	}
	<-backedUp
	return nil
}
