// SPDX-License-Identifier: AGPL-3.0-only
package smb2

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

// These real TCP sessions deliberately install a known signing key instead of
// negotiating NTLM. This isolates wire locking and native cleanup; the executable
// E2E and auth_wire tests independently cover actual NTLM authentication.
// No filesystem, lock table, metadata backend, or object store is mocked here.
type minioLockPeer struct {
	client               net.Conn
	server               *conn
	wire                 transport
	signer               *session
	sessionID, messageID uint64
	done                 chan struct{}
	cleanupErr           error
}

func newMinioLockPeer(t *testing.T, d *Server, fs *smbfs.FS, sid uint64) *minioLockPeer {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	client, e := net.Dial("tcp", listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	peer, e := listener.Accept()
	if e != nil {
		client.Close()
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{t: direct(peer), ctx: ctx, cancel: cancel, serverCtx: d, serverState: STATE_SESSION_ACTIVE, requireSigning: true, dialect: SMB210, account: openAccount(64), outstandingRequests: newOutstandingRequests(), rdone: make(chan struct{}, 1), wdone: make(chan struct{}, 1), write: make(chan []byte, 10), werr: make(chan error, 1), sessions: map[uint64]*session{}, treeMapById: map[uint32]treeOps{}}
	key := []byte("synthetic-native-minio-lock-signing-key")
	sess := &session{conn: c, sessionId: sid, signer: hmac.New(sha256.New, key), verifier: hmac.New(sha256.New, key), treeConnTables: map[uint32]*treeConn{}}
	c.registerSession(sess)
	c.enableSession()
	tree := &fileTree{treeConn: treeConn{session: sess, treeId: 1, path: "native-lock-share"}, fs: fs, ioReadSem: make(chan struct{}, 2), ioWriteSem: make(chan struct{}, 2)}
	c.treeMapById[1] = tree
	q := &minioLockPeer{client: client, server: c, wire: direct(client), sessionID: sid, messageID: 1, done: make(chan struct{})}
	q.signer = &session{signer: hmac.New(sha256.New, key), verifier: hmac.New(sha256.New, key)}
	c.transportWG.Add(2)
	go func() { defer c.transportWG.Done(); c.runReciever() }()
	go func() { defer c.transportWG.Done(); c.runSender() }()
	go func() {
		defer close(q.done)
		_ = c.Run()
		c.shutdown()
		q.cleanupErr = c.closeTreeHandles(nil)
		c.lockWG.Wait()
		c.transportWG.Wait()
	}()
	t.Cleanup(func() { q.disconnect(t) })
	return q
}

func (q *minioLockPeer) disconnect(t *testing.T) {
	t.Helper()
	_ = q.client.Close()
	select {
	case <-q.done:
		if q.cleanupErr != nil {
			t.Errorf("native disconnect cleanup: %v", q.cleanupErr)
		}
	case <-time.After(5 * time.Second):
		q.server.shutdown()
		t.Fatal("native SMB disconnect did not drain")
	}
}
func (q *minioLockPeer) exchange(t *testing.T, b []byte, want NtStatus) []byte {
	t.Helper()
	p := PacketCodec(b)
	p.SetSessionId(q.sessionID)
	p.SetTreeId(1)
	p.SetMessageId(q.messageID)
	p.SetCreditCharge(1)
	p.SetCreditRequest(1)
	q.messageID++
	q.signer.sign(b)
	if e := q.client.SetDeadline(time.Now().Add(10 * time.Second)); e != nil {
		t.Fatal(e)
	}
	if _, e := q.wire.Write(b); e != nil {
		t.Fatal(e)
	}
	for {
		size, e := q.wire.ReadSize()
		if e != nil {
			t.Fatal(e)
		}
		response := make([]byte, size)
		if _, e = q.wire.Read(response); e != nil {
			t.Fatal(e)
		}
		r := PacketCodec(response)
		if r.IsInvalid() || r.MessageId() != p.MessageId() || r.SessionId() != q.sessionID {
			t.Fatal("invalid/mismatched native SMB response")
		}
		if !q.signer.verify(response) {
			t.Fatal("invalid native SMB response signature")
		}
		// Ordinary standalone native READ/WRITE send a signed interim pending
		// response followed by their signed asynchronous completion.
		if r.Status() == uint32(STATUS_PENDING) {
			continue
		}
		if r.Status() != uint32(want) {
			t.Fatalf("SMB command %#x status %#x, want %#x", p.Command(), r.Status(), want)
		}
		return r.Data()
	}
}
func (q *minioLockPeer) request(t *testing.T, r Packet, want NtStatus) []byte {
	t.Helper()
	b := make([]byte, r.Size())
	r.Encode(b)
	return q.exchange(t, b, want)
}
func (q *minioLockPeer) open(t *testing.T) *FileId {
	t.Helper()
	b := q.request(t, &CreateRequest{Name: "locked-data", CreateDisposition: FILE_OPEN_IF, DesiredAccess: GENERIC_READ | GENERIC_WRITE, ShareAccess: FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, CreateOptions: FILE_NON_DIRECTORY_FILE}, STATUS_SUCCESS)
	r := CreateResponseDecoder(b)
	if r.IsInvalid() {
		t.Fatal("invalid native CREATE response")
	}
	return r.FileId().Decode()
}
func (q *minioLockPeer) lock(t *testing.T, id *FileId, offset, length uint64, flags uint32, want NtStatus) {
	t.Helper()
	b := make([]byte, 112)
	p := PacketCodec(b)
	p.SetProtocolId()
	p.SetStructureSize()
	p.SetCommand(SMB2_LOCK)
	binary.LittleEndian.PutUint16(b[64:66], 48)
	binary.LittleEndian.PutUint16(b[66:68], 1)
	id.Encode(b[72:88])
	binary.LittleEndian.PutUint64(b[88:96], offset)
	binary.LittleEndian.PutUint64(b[96:104], length)
	binary.LittleEndian.PutUint32(b[104:108], flags)
	q.exchange(t, b, want)
}

func TestMinIOSMBNativeByteRangeLocks(t *testing.T) {
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("needs MinIO: run scripts/check.sh")
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "http" || u.Host == "" {
		t.Fatal("test requires disposable HTTP MinIO endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/minio/health/ready", nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("MinIO readiness deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	pathStyle := true
	raw, e := object.NewS3(object.S3Options{Bucket: fmt.Sprintf("smb-locks-%d", time.Now().UnixNano()), Region: "us-east-1", Endpoint: endpoint, PathStyle: &pathStyle, AccessKey: "s3smb-test-access", SecretKey: "s3smb-test-secret-only"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if closer, ok := raw.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	if e = raw.Create(ctx); e != nil {
		t.Fatal(e)
	}
	format, e := storage.NewFormat("native-locks", false, 14)
	if e != nil {
		t.Fatal(e)
	}
	blob, e := storage.OpenVolume(ctx, raw, format, "", true)
	if e != nil {
		t.Fatal(e)
	}
	mc := meta.DefaultConf()
	mc.NoBGJob = true
	// This isolated wire fixture never runs destructive maintenance. It is not a
	// substitute for the executable's initial metadata-backup startup gate.
	denied := func() error { return errors.New("destructive maintenance disabled in lock fixture") }
	mc.CheckMaintenance = denied
	m, e := storage.OpenMetadata(filepath.Join(t.TempDir(), "metadata.db"), mc)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	if e = m.Init(format, true); e != nil {
		t.Fatal(e)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0770}
	if er := m.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); er != 0 {
		t.Fatal(er)
	}
	zero := uint64(0)
	runtime, e := storage.OpenFilesystem(m, blob, format, t.TempDir(), &zero, denied)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := runtime.Close(); e != nil {
			t.Error(e)
		}
	})
	if e = m.NewSession(true); e != nil {
		t.Fatal(e)
	}
	fs, e := smbfs.New(runtime.FS, false)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := fs.Shutdown(); e != nil {
			t.Error(e)
		}
	})
	server := NewServer(&ServerConfig{}, nil, nil)
	a := newMinioLockPeer(t, server, fs, 1)
	b := newMinioLockPeer(t, server, fs, 2)
	aid := a.open(t)
	data := []byte("native MinIO bytes exercised by signed SMB locking")
	a.request(t, &WriteRequest{FileId: aid, Data: data, Flags: SMB2_WRITEFLAG_WRITE_THROUGH}, STATUS_SUCCESS)
	bid := b.open(t)
	if aid.NodeId() != bid.NodeId() || aid.HandleId() == bid.HandleId() {
		t.Fatal("fixture did not open two owners of the same native inode")
	}
	objects, e := object.ListAll(ctx, blob, "chunks/", "", true, false)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for obj := range objects {
		if obj == nil {
			t.Fatal("MinIO listing failed")
		}
		if !obj.IsDir() && obj.Size() > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("SMB write did not publish a native chunk to actual MinIO")
	}
	nativeLock := func(want uint32) {
		t.Helper()
		typ := uint32(syscall.F_WRLCK)
		start, end := uint64(0), uint64(7)
		var pid uint32
		if er := m.Getlk(meta.Background(), meta.Ino(aid.NodeId()), math.MaxUint64, &typ, &start, &end, &pid); er != 0 {
			t.Fatal(er)
		}
		if typ != want {
			t.Fatalf("actual native lock type %d, want %d", typ, want)
		}
	}
	const exclusive = SMB2_LOCKFLAG_EXCLUSIVE_LOCK | SMB2_LOCKFLAG_FAIL_IMMEDIATELY
	const shared = SMB2_LOCKFLAG_SHARED_LOCK | SMB2_LOCKFLAG_FAIL_IMMEDIATELY
	a.lock(t, aid, 0, 8, exclusive, STATUS_SUCCESS)
	nativeLock(syscall.F_WRLCK)
	b.lock(t, bid, 0, 8, exclusive, STATUS_LOCK_NOT_GRANTED)
	b.request(t, &ReadRequest{FileId: bid, Length: 8}, STATUS_FILE_LOCK_CONFLICT)
	b.lock(t, bid, 16, 8, exclusive, STATUS_SUCCESS)
	b.lock(t, bid, 16, 8, SMB2_LOCKFLAG_UNLOCK, STATUS_SUCCESS)
	a.lock(t, aid, 0, 8, SMB2_LOCKFLAG_UNLOCK, STATUS_SUCCESS)
	nativeLock(syscall.F_UNLCK)
	a.lock(t, aid, 0, 8, shared, STATUS_SUCCESS)
	b.lock(t, bid, 0, 8, shared, STATUS_SUCCESS)
	nativeLock(syscall.F_RDLCK)
	read := ReadResponseDecoder(b.request(t, &ReadRequest{FileId: bid, Length: 8}, STATUS_SUCCESS))
	if read.IsInvalid() || !bytes.Equal(read.Data(), data[:8]) {
		t.Fatal("shared-locked native MinIO read returned wrong bytes")
	}
	b.request(t, &WriteRequest{FileId: bid, Data: []byte("X")}, STATUS_FILE_LOCK_CONFLICT)
	b.lock(t, bid, 0, 8, SMB2_LOCKFLAG_UNLOCK, STATUS_SUCCESS)
	b.lock(t, bid, 0, 8, exclusive, STATUS_LOCK_NOT_GRANTED)
	a.lock(t, aid, 0, 8, SMB2_LOCKFLAG_UNLOCK, STATUS_SUCCESS)
	b.lock(t, bid, 0, 8, exclusive, STATUS_SUCCESS)
	nativeLock(syscall.F_WRLCK)
	// No SMB CLOSE/UNLOCK: native connection cleanup must release B's owner.
	b.disconnect(t)
	nativeLock(syscall.F_UNLCK)
	a.lock(t, aid, 0, 8, exclusive, STATUS_SUCCESS)
	c := newMinioLockPeer(t, server, fs, 3)
	cid := c.open(t)
	c.lock(t, cid, 0, 8, exclusive, STATUS_LOCK_NOT_GRANTED)
	a.request(t, &CloseRequest{FileId: aid}, STATUS_SUCCESS)
	nativeLock(syscall.F_UNLCK)
	c.lock(t, cid, 0, 8, exclusive, STATUS_SUCCESS)
	c.request(t, &CloseRequest{FileId: cid}, STATUS_SUCCESS)
	nativeLock(syscall.F_UNLCK)
	t.Log("signed TCP LOCK/READ/WRITE/CLOSE: real smbfs, SQLite lock rows and MinIO chunks; known-key sessions, not an NTLM handshake test")
}
