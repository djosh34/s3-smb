package smb2

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func TestMalformedWriteWireStatus(t *testing.T) {
	for _, mutate := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint16(b[66:68], 0) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[68:72], 0xffffffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[64:66], 0) },
	} {
		f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
		tree, id, out := wireTree(t, f)
		b := requestBytes(&WriteRequest{FileId: id, Data: []byte("fixture")})
		mutate(b)
		if err := tree.write(nil, b); err != nil {
			t.Fatal(err)
		}
		if status := wireStatus(t, out); status != STATUS_INVALID_PARAMETER {
			t.Fatalf("status %v", status)
		}
		if f.written != 0 {
			t.Fatal("malformed write reached backend")
		}
	}
}

func TestMalformedCompoundDisconnectsWire(t *testing.T) {
	_, l := startWireServer(t, "")
	for _, offset := range []uint32{1, 64, 0xfffffff8} {
		c, e := net.Dial("tcp", l.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		req := requestBytes(&EchoResponse{})
		binary.LittleEndian.PutUint32(req[20:24], offset)
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(req)))
		if _, e = c.Write(append(header[:], req...)); e != nil {
			t.Fatal(e)
		}
		b := make([]byte, 4)
		_, e = c.Read(b)
		c.Close()
		if e == nil {
			t.Fatal("malformed compound accepted")
		}
		if n, ok := e.(net.Error); ok && n.Timeout() {
			t.Fatal("malformed compound hung connection")
		}
	}
}
