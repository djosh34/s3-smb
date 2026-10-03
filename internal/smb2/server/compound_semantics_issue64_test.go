//go:build issue64probe

package smb2

// Opt-in diagnostic expectations, intentionally red on the investigated base.
// These neighboring semantic defects are NOT reproductions of issue64's
// historical short-client-header failure. Run with -tags issue64probe.
import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func TestIssue64ProbeRelatedExistingFileID(t *testing.T) {
	c, s, _, id := boundaryFixture(t, nil)
	req := boundaryRequests(s, 10, true, 0, 0, &FlushRequest{FileId: id}, &FlushRequest{FileId: &INVALID_GUID})
	boundarySend(t, c, false, req)
	parts := boundaryReceive(t, c, s)
	if len(parts) != 2 {
		t.Fatalf("members=%d", len(parts))
	}
	for i, p := range parts {
		if PacketCodec(p).Status() != 0 {
			t.Fatalf("member %d: inherited existing FileId failed status=%#x", i, PacketCodec(p).Status())
		}
	}
}

func TestIssue64ControlExplicitExistingFileID(t *testing.T) {
	c, s, _, id := boundaryFixture(t, nil)
	req := boundaryRequests(s, 10, true, 0, 0, &FlushRequest{FileId: id}, &FlushRequest{FileId: id})
	boundarySend(t, c, false, req)
	parts := boundaryReceive(t, c, s)
	if len(parts) != 2 {
		t.Fatalf("members=%d", len(parts))
	}
	for i, p := range parts {
		if PacketCodec(p).Status() != 0 {
			t.Fatalf("member %d status=%#x", i, PacketCodec(p).Status())
		}
	}
}

func TestIssue64ControlAsyncSingleton(t *testing.T) {
	c, s, _, id := boundaryFixtureType(t, nil, vfs.FileTypeDirectory)
	boundarySend(t, c, false, boundaryRequests(s, 10, false, 0, 0, &boundaryNotifyRequest{id: id}))
	pending := boundaryReceive(t, c, s)
	if len(pending) != 1 || PacketCodec(pending[0]).Status() != uint32(STATUS_PENDING) {
		t.Fatal("invalid singleton pending")
	}
	final := boundaryReceive(t, c, s)
	if len(final) != 1 || PacketCodec(final[0]).Status() != uint32(STATUS_ACCESS_DENIED) || PacketCodec(final[0]).MessageId() != 10 || binary.LittleEndian.Uint64(final[0][32:40]) != binary.LittleEndian.Uint64(pending[0][32:40]) {
		t.Fatal("invalid singleton final")
	}
}

// Minimal fixed-size CHANGE_NOTIFY encoder; reuse only the ordinary header
// encoding and overwrite command/body before signing. No live auth traffic.
type boundaryNotifyRequest struct {
	PacketHeader
	id *FileId
}

func (p *boundaryNotifyRequest) Header() *PacketHeader { return &p.PacketHeader }
func (p *boundaryNotifyRequest) Size() int             { return 96 }
func (p *boundaryNotifyRequest) Encode(b []byte) {
	(&FlushRequest{PacketHeader: p.PacketHeader, FileId: p.id}).Encode(b)
	binary.LittleEndian.PutUint16(b[12:14], SMB2_CHANGE_NOTIFY)
	clear(b[64:])
	binary.LittleEndian.PutUint16(b[64:66], 32)
	binary.LittleEndian.PutUint32(b[68:72], 4096)
	p.id.Encode(b[72:88])
	binary.LittleEndian.PutUint32(b[88:92], 1) // FILE_NOTIFY_CHANGE_FILE_NAME
}

func TestIssue64ProbeAsyncCompoundCompletion(t *testing.T) {
	c, s, _, id := boundaryFixtureType(t, nil, vfs.FileTypeDirectory)
	req := boundaryRequests(s, 10, true, 0, 0, &FlushRequest{FileId: id}, &boundaryNotifyRequest{id: id})
	boundarySend(t, c, false, req)
	pending := boundaryReceive(t, c, s)
	if len(pending) != 2 || PacketCodec(pending[0]).Status() != 0 || PacketCodec(pending[1]).Status() != uint32(STATUS_PENDING) {
		t.Fatal("unexpected initial compound responses")
	}
	if PacketCodec(pending[1]).MessageId() != 11 {
		t.Fatal("wrong pending ID")
	}
	async := binary.LittleEndian.Uint64(pending[1][32:40])
	// The current backend completes CHANGE_NOTIFY with ACCESS_DENIED after 5s.
	// Whatever its status, a final completion must be reachable by NextCommand
	// and must not replay the already completed FLUSH or pending notification.
	raw := boundaryReadFrame(t, c)
	t.Logf("final frame actual body=%d (read exactly declared prefix)", len(raw))
	// Metadata only: these known candidate boundaries are predicted by the
	// stale-context hypothesis, not a general magic-byte reassembly heuristic.
	for _, off := range []int{0, 72, 152} {
		if off+64 > len(raw) || !bytes.Equal(raw[off:off+4], []byte{0xfe, 'S', 'M', 'B'}) {
			continue
		}
		h := PacketCodec(raw[off:])
		t.Logf("candidate offset=%d id=%d status=%#x next=%d", off, h.MessageId(), h.Status(), h.NextCommand())
	}
	if len(raw) == 225 {
		for _, span := range [][2]int{{0, 72}, {72, 145}, {152, 225}, {72, 225}} {
			b := bytes.Clone(raw[span[0]:span[1]])
			sig := bytes.Clone(b[48:64])
			clear(b[48:64])
			mac := hmac.New(sha256.New, []byte("synthetic-boundary-fixture-key"))
			mac.Write(b)
			t.Logf("independent HMAC span=%d:%d valid=%t", span[0], span[1], hmac.Equal(sig, mac.Sum(nil)[:16]))
		}
	}
	final := boundaryMembers(t, raw, s)
	for i, p := range final {
		h := PacketCodec(p)
		t.Logf("final member %d id=%d status=%#x next=%d bytes=%d", i, h.MessageId(), h.Status(), h.NextCommand(), len(p))
	}
	if len(final) != 1 || PacketCodec(final[0]).MessageId() != 11 || PacketCodec(final[0]).Status() == uint32(STATUS_PENDING) || binary.LittleEndian.Uint64(final[0][32:40]) != async {
		t.Fatal("async final was replayed/hidden by stale compound response assembly")
	}
}
