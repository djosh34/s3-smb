package smb2

// Issue64 investigation: real TCP receiver/dispatcher/sender, synthetic known-key
// session and deterministic VFS. No historical failing stream is replayed here.
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

type boundaryFS struct {
	*deleteDispositionFS
	mu    sync.Mutex
	data  map[uint64][]byte
	gates map[uint64]chan struct{}
	stop  chan struct{}
}

func (f *boundaryFS) Write(_ vfs.VfsHandle, b []byte, off uint64, _ int) (int, error) {
	if gate := f.gates[off]; gate != nil {
		select {
		case <-gate:
		case <-f.stop:
			return 0, context.Canceled
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[off] = bytes.Clone(b)
	return len(b), nil
}
func (f *boundaryFS) Read(_ vfs.VfsHandle, b []byte, off uint64, _ int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copy(b, f.data[off]), nil
}
func (f *boundaryFS) Flush(vfs.VfsHandle) error { return nil }

func boundaryFixture(t *testing.T, gates map[uint64]chan struct{}) (net.Conn, *session, *boundaryFS, *FileId) {
	t.Helper()
	return boundaryFixtureType(t, gates, vfs.FileTypeRegularFile)
}

func boundaryFixtureType(t *testing.T, gates map[uint64]chan struct{}, fileType vfs.FileType) (net.Conn, *session, *boundaryFS, *FileId) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	peer, err := l.Accept()
	l.Close()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	client.SetDeadline(time.Now().Add(10 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{t: direct(peer), ctx: ctx, cancel: cancel, serverCtx: NewServer(&ServerConfig{}, nil, nil), serverState: STATE_SESSION_ACTIVE, requireSigning: true, dialect: SMB210, account: openAccount(128), outstandingRequests: newOutstandingRequests(), rdone: make(chan struct{}, 1), wdone: make(chan struct{}, 1), write: make(chan []byte, 32), werr: make(chan error, 1), sessions: map[uint64]*session{}, treeMapById: map[uint32]treeOps{}}
	key := []byte("synthetic-boundary-fixture-key")
	s := &session{conn: c, sessionId: 1, signer: hmac.New(sha256.New, key), verifier: hmac.New(sha256.New, key), treeConnTables: map[uint32]*treeConn{}}
	c.registerSession(s)
	c.enableSession()
	fs := &boundaryFS{deleteDispositionFS: newDeleteDispositionFS(fileType), data: map[uint64][]byte{}, gates: gates, stop: make(chan struct{})}
	tree := &fileTree{treeConn: treeConn{session: s, treeId: 1}, fs: fs, ioWriteSem: make(chan struct{}, 16), ioReadSem: make(chan struct{}, 16)}
	c.treeMapById[1] = tree
	id := &FileId{}
	id.SetHandleId(7)
	id.SetNodeId(42)
	open := &Open{fileId: 7, durableFileId: 42, tree: &tree.treeConn, session: s}
	if fileType == vfs.FileTypeDirectory {
		open.fileAttributes = FILE_ATTRIBUTE_DIRECTORY
	}
	c.serverCtx.addOpen(open)
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
		close(fs.stop)
		client.Close()
		c.shutdown()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("boundary fixture failed to drain")
		}
	})
	return client, &session{signer: hmac.New(sha256.New, key), verifier: hmac.New(sha256.New, key)}, fs, id
}

// DataOffset gaps and NextCommand padding are distinct. Both are signed, and
// WRITE padding must never reach the VFS as data. IDs are unique across frames.
func boundaryRequests(s *session, firstID uint64, related bool, gap, extraPad int, requests ...Packet) []byte {
	var result []byte
	for i, r := range requests {
		h := r.Header()
		h.MessageId = firstID + uint64(i)
		h.SessionId = 1
		h.TreeId = 1
		h.CreditCharge = 1
		h.CreditRequestResponse = 1
		if i > 0 && related {
			h.Flags = SMB2_FLAGS_RELATED_OPERATIONS
			h.SessionId = ^uint64(0)
			h.TreeId = ^uint32(0)
		}
		b := requestBytes(r)
		if _, ok := r.(*WriteRequest); ok && gap > 0 {
			moved := make([]byte, len(b)+gap)
			copy(moved, b[:112])
			copy(moved[112+gap:], b[112:])
			b = moved
			binary.LittleEndian.PutUint16(b[66:68], uint16(112+gap))
		}
		if i < len(requests)-1 {
			size := Align(len(b), 8) + extraPad
			b = append(b, make([]byte, size-len(b))...)
			binary.LittleEndian.PutUint32(b[20:24], uint32(size))
		}
		s.sign(b)
		result = append(result, b...)
	}
	return result
}
func boundarySend(t *testing.T, c net.Conn, fragment bool, bodies ...[]byte) {
	t.Helper()
	var stream []byte
	for _, b := range bodies {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(b)))
		stream = append(stream, prefix[:]...)
		stream = append(stream, b...)
	}
	sizes := []int{1, 2, 3, 7, 257, 65536}
	for i := 0; len(stream) > 0; i++ {
		n := len(stream)
		if fragment && n > sizes[i%len(sizes)] {
			n = sizes[i%len(sizes)]
		}
		wrote, err := c.Write(stream[:n])
		if err != nil {
			t.Fatal(err)
		}
		if wrote != n {
			t.Fatalf("short client write %d/%d", wrote, n)
		}
		stream = stream[n:]
	}
}

// Independent framing and member slicing, not the production splitRequests.
func boundaryReceive(t *testing.T, c net.Conn, s *session) [][]byte {
	t.Helper()
	return boundaryMembers(t, boundaryReadFrame(t, c), s)
}

func boundaryReadFrame(t *testing.T, c net.Conn) []byte {
	t.Helper()
	var prefix [4]byte
	if _, err := io.ReadFull(c, prefix[:]); err != nil {
		t.Fatal(err)
	}
	size := int(binary.BigEndian.Uint32(prefix[:]))
	if prefix[0] != 0 || size < 64 || size > 8<<20 {
		t.Fatalf("invalid response frame length=%d type=%d", size, prefix[0])
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func boundaryMembers(t *testing.T, b []byte, s *session) [][]byte {
	t.Helper()
	var parts [][]byte
	for off := 0; off < len(b); {
		if len(b)-off < 64 || !bytes.Equal(b[off:off+4], []byte{0xfe, 'S', 'M', 'B'}) {
			t.Fatalf("invalid response header at offset %d of %d", off, len(b))
		}
		next := int(binary.LittleEndian.Uint32(b[off+20 : off+24]))
		end := len(b)
		if next != 0 {
			if next < 64 || next%8 != 0 || off+next+64 > len(b) {
				t.Fatalf("invalid NextCommand=%d offset=%d frame=%d", next, off, len(b))
			}
			end = off + next
		}
		part := b[off:end]
		if !s.verify(part) {
			t.Fatalf("invalid signature member=%d size=%d", len(parts), len(part))
		}
		parts = append(parts, part)
		off = end
	}
	return parts
}
func boundaryPattern(n, seed int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + seed) % 251)
	}
	return b
}

func TestCompoundBoundaryOffsetsPaddingWire(t *testing.T) {
	for _, n := range []int{0, 1, 7, 8, 9, 65535, 65536, 65537, 1 << 20} {
		for _, gap := range []int{0, 1, 7, 16, 4096} {
			t.Run(fmt.Sprintf("bytes%d/gap%d", n, gap), func(t *testing.T) {
				c, s, fs, id := boundaryFixture(t, nil)
				a, b := boundaryPattern(n, 3), boundaryPattern(n+1, 9)
				compound := boundaryRequests(s, 10, true, gap, 24,
					&WriteRequest{FileId: id, Offset: 0, Data: a}, &WriteRequest{FileId: id, Offset: 2 << 20, Data: b}, &FlushRequest{FileId: id},
					&ReadRequest{FileId: id, Offset: 0, Length: uint32(len(a))}, &ReadRequest{FileId: id, Offset: 2 << 20, Length: uint32(len(b))})
				following := boundaryRequests(s, 20, false, 0, 0, &FlushRequest{FileId: id})
				boundarySend(t, c, true, compound, following)
				parts := boundaryReceive(t, c, s)
				if len(parts) != 5 {
					t.Fatalf("members=%d want=5", len(parts))
				}
				for i, p := range parts {
					h := PacketCodec(p)
					if h.MessageId() != 10+uint64(i) || h.Status() != 0 {
						t.Fatalf("member %d id=%d status=%#x", i, h.MessageId(), h.Status())
					}
					if h.Flags()&SMB2_FLAGS_ASYNC_COMMAND != 0 {
						t.Fatalf("compound member %d unexpectedly async", i)
					}
					if i < 2 {
						d := WriteResponseDecoder(h.Data())
						want := len(a)
						if i == 1 {
							want = len(b)
						}
						if d.IsInvalid() || int(d.Count()) != want {
							t.Fatalf("write %d incorrect count", i)
						}
					}
					if i >= 3 {
						d := ReadResponseDecoder(h.Data())
						want := a
						if i == 4 {
							want = b
						}
						if d.IsInvalid() || !bytes.Equal(d.Data(), want) {
							t.Fatalf("read %d incorrect body/offset", i)
						}
					}
				}
				tail := boundaryReceive(t, c, s)
				if len(tail) != 1 || PacketCodec(tail[0]).MessageId() != 20 || PacketCodec(tail[0]).Status() != 0 {
					t.Fatal("following frame misaligned")
				}
				fs.mu.Lock()
				defer fs.mu.Unlock()
				if !bytes.Equal(fs.data[0], a) || !bytes.Equal(fs.data[2<<20], b) {
					t.Fatal("VFS received wrong WRITE data/padding")
				}
			})
		}
	}
}

func TestCompoundBoundaryPipelinedAsyncWire(t *testing.T) {
	const count = 8
	gates := map[uint64]chan struct{}{}
	for i := 0; i < count; i++ {
		gates[uint64(i)<<21] = make(chan struct{})
	}
	c, s, fs, id := boundaryFixture(t, gates)
	var frames [][]byte
	lengths := []int{1, 7, 8, 9, 65535, 65536, 65537, 1 << 20}
	for i, n := range lengths {
		frames = append(frames, boundaryRequests(s, uint64(100+i), false, i, 0, &WriteRequest{FileId: id, Offset: uint64(i) << 21, Data: boundaryPattern(n, i)}))
	}
	boundarySend(t, c, false, frames...)
	async := map[uint64]uint64{}
	for i := 0; i < count; i++ {
		parts := boundaryReceive(t, c, s)
		if len(parts) != 1 {
			t.Fatalf("pending members=%d", len(parts))
		}
		p := PacketCodec(parts[0])
		mid := p.MessageId()
		if p.Status() != uint32(STATUS_PENDING) || p.Flags()&SMB2_FLAGS_ASYNC_COMMAND == 0 || mid < 100 || mid >= 100+count {
			t.Fatalf("invalid pending id=%d status=%#x flags=%#x", mid, p.Status(), p.Flags())
		}
		if _, ok := async[mid]; ok {
			t.Fatalf("duplicate pending id=%d", mid)
		}
		async[mid] = binary.LittleEndian.Uint64(parts[0][32:40])
	}
	// Deliberately reverse completions; no sleeps or assumed goroutine ordering.
	for i := count - 1; i >= 0; i-- {
		close(gates[uint64(i)<<21])
		parts := boundaryReceive(t, c, s)
		if len(parts) != 1 {
			t.Fatalf("final members=%d", len(parts))
		}
		p := PacketCodec(parts[0])
		mid := uint64(100 + i)
		if p.MessageId() != mid || p.Status() != 0 || p.Flags()&SMB2_FLAGS_ASYNC_COMMAND == 0 || binary.LittleEndian.Uint64(parts[0][32:40]) != async[mid] {
			t.Fatalf("invalid final id=%d want=%d status=%#x flags=%#x", p.MessageId(), mid, p.Status(), p.Flags())
		}
		d := WriteResponseDecoder(p.Data())
		if d.IsInvalid() || int(d.Count()) != lengths[i] {
			t.Fatalf("write count mismatch id=%d", mid)
		}
		if len(parts[0]) != 81 {
			t.Fatalf("observed WRITE response size changed: %d", len(parts[0]))
		}
	}
	// A subsequent compound READ+FLUSH checks both byte ownership and the next
	// frame boundary after interleaved pending/final response production.
	for i, n := range lengths {
		req := boundaryRequests(s, uint64(200+2*i), true, 0, 8, &ReadRequest{FileId: id, Offset: uint64(i) << 21, Length: uint32(n)}, &FlushRequest{FileId: id})
		boundarySend(t, c, true, req)
		parts := boundaryReceive(t, c, s)
		if len(parts) != 2 || PacketCodec(parts[0]).Status() != 0 || PacketCodec(parts[1]).Status() != 0 {
			t.Fatal("readback compound failed")
		}
		d := ReadResponseDecoder(PacketCodec(parts[0]).Data())
		if d.IsInvalid() || !bytes.Equal(d.Data(), boundaryPattern(n, i)) {
			t.Fatalf("readback data mismatch id=%d", i)
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.data) != count {
		t.Fatalf("writes=%d", len(fs.data))
	}
}
