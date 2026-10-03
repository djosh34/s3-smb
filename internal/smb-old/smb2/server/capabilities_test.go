package smb2

import (
	"encoding/binary"
	"testing"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

func TestNativeAppleCapabilitiesPreserved(t *testing.T) {
	tree, _, _ := wireTree(t, &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)})
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b, AAPL_SERVER_QUERY)
	enc, err := tree.handleAAPLCC(b)
	if err != nil {
		t.Fatal(err)
	}
	rsp := enc.(CreateContext).Data.(*AAPLServerQueryResponse)
	if rsp.VolumeCaps&AAPL_SUPPORTS_FULL_SYNC == 0 || rsp.ServerCaps&AAPL_SUPPORTS_READDIR_ATTR == 0 || rsp.ServerCaps&AAPL_UNIX_BASED == 0 {
		t.Fatal("native Apple capabilities changed")
	}
	// This is capability preservation, not evidence of a Time Machine backup.
}

func TestMalformedCreateOffsets(t *testing.T) {
	for _, mutate := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint16(b[108:110], 0) },
		func(b []byte) {
			binary.LittleEndian.PutUint32(b[112:116], 0xfffffff8)
			binary.LittleEndian.PutUint32(b[116:120], 100)
		},
	} {
		f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
		tree, _, out := wireTree(t, f)
		b := requestBytes(&CreateRequest{Name: "fixture"})
		mutate(b)
		if err := tree.create(nil, b); err != nil {
			t.Fatal(err)
		}
		if status := wireStatus(t, out); status != STATUS_INVALID_PARAMETER {
			t.Fatal(status)
		}
	}
	for _, b := range [][]byte{make([]byte, 1), make([]byte, 15), make([]byte, 16)} {
		if validCreateContexts(b) {
			t.Fatal("invalid create context accepted")
		}
	}
}
