package smb2

import (
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

type creditResponseFS struct {
	*faultFS
	readErr error
}

func (f *creditResponseFS) Read(_ vfs.VfsHandle, b []byte, _ uint64, _ int) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return len(b), nil
}

func receiveCreditResponse(t *testing.T, out <-chan []byte) PacketCodec {
	t.Helper()
	select {
	case p := <-out:
		return PacketCodec(p)
	case <-time.After(2 * time.Second):
		t.Fatal("response timeout")
		return nil
	}
}

// These exercise the response handlers, not a socket, negotiated credit window,
// or Time Machine. Backend errors and conflicting locks are normal I/O outcomes.
func TestCreditResponseAsyncIO(t *testing.T) {
	for _, op := range []string{"read", "write"} {
		for _, result := range []string{"success", "backend-error", "lock-conflict"} {
			t.Run(op+"/"+result, func(t *testing.T) {
				f := &creditResponseFS{faultFS: &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}}
				tree, id, out := wireTree(t, f)
				tree.ioReadSem = make(chan struct{}, 1)
				tree.ioWriteSem = make(chan struct{}, 1)
				wantStatus := uint32(0)
				switch result {
				case "backend-error":
					f.readErr, f.writeErr = io.EOF, syscall.EIO
					wantStatus = uint32(STATUS_END_OF_FILE)
					if op == "write" {
						wantStatus = uint32(STATUS_IO_DEVICE_ERROR)
					}
				case "lock-conflict":
					tree.conn.serverCtx.addOpen(&Open{
						fileId: 8, durableFileId: id.NodeId(), tree: &tree.treeConn, session: tree.session,
						byteRangeLocks: []smbByteRangeLock{{offset: 0, length: 1, exclusive: true}},
					})
					wantStatus = uint32(STATUS_FILE_LOCK_CONFLICT)
				}
				var req Packet = &ReadRequest{FileId: id, Length: 1048576}
				if op == "write" {
					req = &WriteRequest{FileId: id, Data: make([]byte, 1048576)}
				}
				h := req.Header()
				h.MessageId, h.CreditCharge, h.CreditRequestResponse = 100, 16, 16
				h.SessionId, h.TreeId = tree.session.sessionId, tree.treeId
				var err error
				if op == "write" {
					err = tree.write(nil, requestBytes(req))
				} else {
					err = tree.read(nil, requestBytes(req))
				}
				if err != nil {
					t.Fatal(err)
				}
				pending, final := receiveCreditResponse(t, out), receiveCreditResponse(t, out)
				tree.conn.ioWG.Wait()
				if pending.Status() != uint32(STATUS_PENDING) || final.Status() != wantStatus {
					t.Fatalf("statuses pending=%08x final=%08x, want final=%08x", pending.Status(), final.Status(), wantStatus)
				}
				if pending.MessageId() != h.MessageId || final.MessageId() != h.MessageId {
					t.Error("response MessageId changed")
				}
				if pending.Flags()&SMB2_FLAGS_ASYNC_COMMAND == 0 || final.Flags()&SMB2_FLAGS_ASYNC_COMMAND == 0 || pending.AsyncId() == 0 || final.AsyncId() != pending.AsyncId() {
					t.Errorf("async identity changed: pending flags=%x final flags=%x match=%t", pending.Flags(), final.Flags(), pending.AsyncId() == final.AsyncId())
				}
				// MS-SMB2 3.3.4.1.2: all async grants belong in the interim reply.
				if pending.CreditResponse() != 16 || final.CreditResponse() != 0 {
					t.Errorf("credits pending=%d final=%d, want 16 and 0", pending.CreditResponse(), final.CreditResponse())
				}
			})
		}
	}
}

func TestCreditResponseCompoundWrite(t *testing.T) {
	f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
	tree, id, out := wireTree(t, f)
	first := &WriteRequest{FileId: id, Data: []byte{1}}
	second := &WriteRequest{FileId: id, Data: []byte{2}}
	for i, req := range []*WriteRequest{first, second} {
		req.MessageId, req.CreditCharge, req.CreditRequestResponse = uint64(i+1), 1, 1
		req.SessionId, req.TreeId = tree.session.sessionId, tree.treeId
	}
	first.NextCommand = uint32(Align(first.Size(), 8))
	compound := make([]byte, int(first.NextCommand)+second.Size())
	first.Encode(compound[:first.NextCommand])
	second.Encode(compound[first.NextCommand:])
	parts, err := splitRequests(compound)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &compoundContext{lastMsgId: second.MessageId}
	for _, part := range parts {
		if err := tree.write(ctx, part); err != nil {
			t.Fatal(err)
		}
	}
	responses, err := splitRequests(receiveCreditResponse(t, out))
	if err != nil || len(responses) != 2 {
		t.Fatalf("response chain: members=%d err=%v", len(responses), err)
	}
	// At the two-credit boundary, this compound spends the entire balance.
	// It must replenish at least one credit; no live client balance is modeled.
	grants := 0
	for i, response := range responses {
		p := PacketCodec(response)
		if p.Status() != 0 || p.MessageId() != uint64(i+1) || p.Flags()&SMB2_FLAGS_ASYNC_COMMAND != 0 {
			t.Fatalf("unexpected synchronous WRITE response %d", i)
		}
		grants += int(p.CreditResponse())
	}
	if grants == 0 {
		t.Error("two-credit client receives zero total grants after spending both credits")
	}
}

func TestCreditResponseNegotiate(t *testing.T) {
	for _, dialect := range []uint16{SMB210, SMB311} {
		for _, requested := range []uint16{0, 1, 16} {
			t.Run(fmt.Sprintf("%x/request-%d", dialect, requested), func(t *testing.T) {
				f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
				tree, _, out := wireTree(t, f)
				tree.conn.serverCtx.authenticator = &NTLMAuthenticator{UserPassword: map[string]string{}}
				req := &NegotiateRequest{Dialects: []uint16{dialect}, SecurityMode: SMB2_NEGOTIATE_SIGNING_ENABLED, Capabilities: SMB2_GLOBAL_CAP_LARGE_MTU}
				req.CreditRequestResponse = requested
				if dialect == SMB311 {
					req.Contexts = []Encoder{&HashContext{HashAlgorithms: []uint16{SHA512}}, &CipherContext{Ciphers: []uint16{AES128GCM}}}
				}
				if err := (&ServerNegotiator{}).negotiate(tree.conn, requestBytes(req)); err != nil {
					t.Fatal(err)
				}
				p := receiveCreditResponse(t, out)
				if p.Status() != 0 || NegotiateResponseDecoder(p.Data()).DialectRevision() != dialect {
					t.Fatal("negotiation did not succeed with the requested dialect")
				}
				// MS-SMB2 3.3.1.2 requires at least one, even when requested=0.
				if got, want := p.CreditResponse(), max(uint16(1), requested); got != want {
					t.Errorf("credits=%d, want %d", got, want)
				}
			})
		}
	}
}
