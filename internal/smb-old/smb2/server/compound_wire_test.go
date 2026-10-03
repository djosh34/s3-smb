package smb2

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"net"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

// A known-key authenticated session isolates compound framing/signing over TCP.
// The independent auth_wire tests exercise the real NTLM handshake.
func compoundWireFixture(t *testing.T) (transport, *session, *faultFS) {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	client, e := net.Dial("tcp", l.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	peer, e := l.Accept()
	l.Close()
	if e != nil {
		t.Fatal(e)
	}
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	d := NewServer(&ServerConfig{}, nil, nil)
	c := &conn{t: direct(peer), ctx: ctx, cancel: cancel, serverCtx: d, serverState: STATE_SESSION_ACTIVE, requireSigning: true, dialect: SMB210, account: openAccount(16), outstandingRequests: newOutstandingRequests(), rdone: make(chan struct{}, 1), wdone: make(chan struct{}, 1), write: make(chan []byte, 10), werr: make(chan error, 1), sessions: map[uint64]*session{}, treeMapById: map[uint32]treeOps{}}
	key := []byte("synthetic-signing-fixture-key")
	s := &session{conn: c, sessionId: 1, signer: hmac.New(sha256.New, key), verifier: hmac.New(sha256.New, key), treeConnTables: map[uint32]*treeConn{}}
	c.registerSession(s)
	c.enableSession()
	fs := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
	tree := &fileTree{treeConn: treeConn{session: s, treeId: 1}, fs: fs}
	c.treeMapById[1] = tree
	c.transportWG.Add(2)
	go func() { defer c.transportWG.Done(); c.runReciever() }()
	go func() { defer c.transportWG.Done(); c.runSender() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run()
		c.shutdown()
		_ = c.closeTreeHandles(nil)
		c.lockWG.Wait()
		c.transportWG.Wait()
	}()
	t.Cleanup(func() {
		client.Close()
		c.shutdown()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("compound connection failed to drain")
		}
	})
	signer := &session{signer: hmac.New(sha256.New, key), verifier: hmac.New(sha256.New, key)}
	return direct(client), signer, fs
}
func signedRelatedRequests(s *session, requests ...Packet) []byte {
	var wire []byte
	for i, r := range requests {
		h := r.Header()
		h.SessionId = 1
		h.TreeId = 1
		h.MessageId = uint64(10 + i)
		h.CreditCharge = 1
		if i > 0 {
			h.Flags = SMB2_FLAGS_RELATED_OPERATIONS
			h.SessionId = ^uint64(0)
			h.TreeId = ^uint32(0)
		}
		size := r.Size()
		if i < len(requests)-1 {
			size = Align(size, 8)
			h.NextCommand = uint32(size)
		}
		b := make([]byte, size)
		r.Encode(b)
		s.sign(b)
		wire = append(wire, b...)
	}
	return wire
}
func TestSignedRelatedCreateWriteFlushCloseWire(t *testing.T) {
	tr, signer, fs := compoundWireFixture(t)
	wire := signedRelatedRequests(signer,
		&CreateRequest{Name: "fixture", CreateDisposition: FILE_OPEN, DesiredAccess: GENERIC_WRITE},
		&WriteRequest{FileId: &INVALID_GUID, Data: []byte("fixture")},
		&FlushRequest{FileId: &INVALID_GUID},
		&CloseRequest{FileId: &INVALID_GUID},
	)
	if _, e := tr.Write(wire); e != nil {
		t.Fatal(e)
	}
	n, e := tr.ReadSize()
	if e != nil {
		t.Fatal(e)
	}
	response := make([]byte, n)
	if _, e = tr.Read(response); e != nil {
		t.Fatal(e)
	}
	parts, e := splitRequests(response)
	if e != nil {
		t.Fatal(e)
	}
	if len(parts) != 4 {
		t.Fatalf("compound responses = %d", len(parts))
	}
	for i, p := range parts {
		if PacketCodec(p).Status() != uint32(STATUS_SUCCESS) {
			t.Fatalf("member %d: status %#x", i, PacketCodec(p).Status())
		}
		if !signer.verify(p) {
			t.Fatalf("member %d: bad response signature", i)
		}
	}
	// Reading the final response establishes that all synchronous compound VFS
	// operations completed. No backend operations remain until test cleanup.
	if fs.written != 1 || fs.flushed != 2 || fs.closed != 1 {
		t.Fatalf("write=%d flush=%d close=%d", fs.written, fs.flushed, fs.closed)
	}
}
func TestRelatedSessionSentinelRequiresLeadingOperation(t *testing.T) {
	for _, related := range []bool{false, true} {
		tr, signer, _ := compoundWireFixture(t)
		r := &FlushRequest{FileId: &INVALID_GUID}
		r.SessionId = ^uint64(0)
		r.TreeId = ^uint32(0)
		r.MessageId = 10
		if related {
			r.Flags = SMB2_FLAGS_RELATED_OPERATIONS
		}
		b := requestBytes(r)
		signer.sign(b)
		if _, e := tr.Write(b); e != nil {
			t.Fatal(e)
		}
		if _, e := tr.ReadSize(); e == nil {
			t.Fatal("sentinel without leading operation accepted")
		}
	}
}
