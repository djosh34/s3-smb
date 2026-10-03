package smb2

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"testing"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

// These use the real TCP dispatcher and a synthetic established SMB210 session.
// CREATE installs the open, including its session, tree and granted access.
// No authentication, negotiation or client credit-window implementation is tested.
func compoundContextSend(t *testing.T, tr transport, signer *session, mid uint64, requests ...Packet) {
	t.Helper()
	var wire []byte
	for i, r := range requests {
		h := r.Header()
		h.MessageId, h.SessionId, h.TreeId = mid+uint64(i), 1, 1
		h.CreditCharge, h.CreditRequestResponse = 1, 1
		if i > 0 {
			h.Flags = SMB2_FLAGS_RELATED_OPERATIONS
			h.SessionId, h.TreeId = ^uint64(0), ^uint32(0)
		}
		size := r.Size()
		if i < len(requests)-1 {
			size = Align(size, 8)
			h.NextCommand = uint32(size)
		}
		b := make([]byte, size)
		r.Encode(b)
		signer.sign(b)
		wire = append(wire, b...)
	}
	if _, err := tr.Write(wire); err != nil {
		t.Fatal(err)
	}
}

// Read and walk response boundaries independently of directTCP/splitRequests.
func compoundContextRead(t *testing.T, tr transport) []byte {
	t.Helper()
	c := tr.(*directTCP).conn
	var prefix [4]byte
	if _, err := io.ReadFull(c, prefix[:]); err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if prefix[0] != 0 || n < 64 || n > 65536 {
		t.Fatalf("unexpected response length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func compoundContextMembers(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var parts [][]byte
	for len(b) != 0 {
		if len(b) < 64 || !bytes.Equal(b[:4], []byte{0xfe, 'S', 'M', 'B'}) {
			t.Fatal("invalid response member header")
		}
		n := int(binary.LittleEndian.Uint32(b[20:24]))
		if n == 0 {
			n = len(b)
		} else if n < 64 || n%8 != 0 || n+64 > len(b) {
			t.Fatal("invalid response NextCommand")
		}
		parts = append(parts, b[:n])
		b = b[n:]
	}
	return parts
}

func compoundContextSignatures(t *testing.T, parts [][]byte) {
	t.Helper()
	for i, part := range parts {
		p := PacketCodec(part)
		// Interim responses SHOULD NOT be signed; verify if they are signed.
		if p.Status() == uint32(STATUS_PENDING) && p.Flags()&SMB2_FLAGS_SIGNED == 0 {
			continue
		}
		b := bytes.Clone(part)
		clear(b[48:64])
		mac := hmac.New(sha256.New, []byte("synthetic-signing-fixture-key"))
		mac.Write(b)
		if p.Flags()&SMB2_FLAGS_SIGNED == 0 || !hmac.Equal(part[48:64], mac.Sum(nil)[:16]) {
			t.Errorf("invalid response signature member=%d bytes=%d", i, len(part))
		}
	}
}

func compoundContextOpen(t *testing.T, tr transport, signer *session, directory bool) *FileId {
	t.Helper()
	r := &CreateRequest{Name: "fixture", CreateDisposition: FILE_OPEN, DesiredAccess: FILE_READ_DATA | FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_READ_ATTRIBUTES}
	if directory {
		r.CreateOptions = FILE_DIRECTORY_FILE
		// These access bits are directory aliases of READ/WRITE/APPEND_DATA.
		r.DesiredAccess = FILE_LIST_DIRECTORY | FILE_ADD_FILE | FILE_ADD_SUBDIRECTORY | FILE_READ_ATTRIBUTES
	}
	compoundContextSend(t, tr, signer, 1, r)
	parts := compoundContextMembers(t, compoundContextRead(t, tr))
	compoundContextSignatures(t, parts)
	if len(parts) != 1 || PacketCodec(parts[0]).Status() != 0 {
		t.Fatal("CREATE did not succeed")
	}
	response := CreateResponseDecoder(PacketCodec(parts[0]).Data())
	if response.IsInvalid() {
		t.Fatal("invalid CREATE response")
	}
	if directory && response.FileAttributes()&FILE_ATTRIBUTE_DIRECTORY == 0 {
		t.Fatal("CREATE did not open a directory")
	}
	return response.FileId().Decode()
}

func TestCompoundExistingFileID(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		name := "explicit"
		if inherited {
			name = "inherited"
		}
		t.Run(name, func(t *testing.T) {
			tr, signer, _ := compoundWireFixture(t)
			id := compoundContextOpen(t, tr, signer, false)
			next := id
			if inherited {
				next = &INVALID_GUID
			}
			compoundContextSend(t, tr, signer, 10, &FlushRequest{FileId: id}, &FlushRequest{FileId: next})
			parts := compoundContextMembers(t, compoundContextRead(t, tr))
			compoundContextSignatures(t, parts)
			if len(parts) != 2 {
				t.Fatalf("response members=%d, want 2", len(parts))
			}
			for i, part := range parts {
				p := PacketCodec(part)
				if p.MessageId() != uint64(10+i) || p.Status() != 0 {
					t.Errorf("FLUSH member=%d message=%d status=%#x", i, p.MessageId(), p.Status())
				}
			}
		})
	}
}
