package smb2

import (
	"encoding/binary"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

type compoundDirectoryFS struct{ *faultFS }

func (*compoundDirectoryFS) OpenDir(string) (vfs.VfsHandle, error) { return 7, nil }

// CHANGE_NOTIFY has no production request encoder. This encodes its fixed
// 32-byte body: 4096 output bytes, a directory FileId and a filename filter.
type compoundNotifyRequest struct {
	PacketHeader
	id *FileId
}

func (r *compoundNotifyRequest) Header() *PacketHeader { return &r.PacketHeader }
func (r *compoundNotifyRequest) Size() int             { return 96 }
func (r *compoundNotifyRequest) Encode(b []byte) {
	(&FlushRequest{PacketHeader: r.PacketHeader, FileId: r.id}).Encode(b)
	binary.LittleEndian.PutUint16(b[12:14], SMB2_CHANGE_NOTIFY)
	clear(b[64:])
	binary.LittleEndian.PutUint16(b[64:66], 32)
	binary.LittleEndian.PutUint32(b[68:72], 4096)
	r.id.Encode(b[72:88])
	binary.LittleEndian.PutUint32(b[88:92], 1) // FILE_NOTIFY_CHANGE_FILE_NAME
}

func TestCompoundNotifyCompletion(t *testing.T) {
	for _, compound := range []bool{false, true} {
		name := "singleton"
		if compound {
			name = "compound"
		}
		t.Run(name, func(t *testing.T) {
			fs := &compoundDirectoryFS{&faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeDirectory)}}
			tr, signer := compoundWireFixtureFS(t, fs)
			if err := tr.(*directTCP).conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			id := compoundContextOpen(t, tr, signer, true)
			requests := []Packet{&compoundNotifyRequest{id: id}}
			if compound {
				requests = append([]Packet{&FlushRequest{FileId: id}}, requests...)
			}
			compoundContextSend(t, tr, signer, 10, requests...)
			pending := compoundContextMembers(t, compoundContextRead(t, tr))
			compoundContextSignatures(t, pending)
			if len(pending) != len(requests) {
				t.Fatal("wrong interim compound length")
			}
			for i, part := range pending {
				want := uint32(STATUS_SUCCESS)
				if i == len(pending)-1 {
					want = uint32(STATUS_PENDING)
				}
				if PacketCodec(part).Status() != want || PacketCodec(part).MessageId() != uint64(10+i) {
					t.Fatal("unexpected interim response")
				}
			}
			p := PacketCodec(pending[len(pending)-1])
			asyncID := binary.LittleEndian.Uint64(p[32:40])
			if p.Flags()&SMB2_FLAGS_ASYNC_COMMAND == 0 || asyncID == 0 {
				t.Fatal("interim response lacks asynchronous identity")
			}
			// The existing handler completes after five seconds. Test assembly,
			// not its notification timeout/status policy or credit accounting.
			final := compoundContextRead(t, tr)
			compoundContextSend(t, tr, signer, 12, &FlushRequest{FileId: id})
			tail := compoundContextMembers(t, compoundContextRead(t, tr))
			compoundContextSignatures(t, tail)
			if len(tail) != 1 || PacketCodec(tail[0]).MessageId() != 12 || PacketCodec(tail[0]).Status() != 0 {
				t.Fatal("following FLUSH did not succeed")
			}
			t.Logf("final body=%d bytes; following signed FLUSH succeeds", len(final))
			parts := compoundContextMembers(t, final)
			compoundContextSignatures(t, parts)
			if len(parts) != 1 {
				t.Fatalf("final response members=%d, want one notification completion without replay", len(parts))
			}
			f := PacketCodec(parts[0])
			if f.MessageId() != p.MessageId() || f.Status() == uint32(STATUS_PENDING) || f.Flags()&SMB2_FLAGS_ASYNC_COMMAND == 0 || binary.LittleEndian.Uint64(f[32:40]) != asyncID {
				t.Fatal("final notification identity or status is incorrect")
			}
		})
	}
}
