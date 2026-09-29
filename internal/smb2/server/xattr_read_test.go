package smb2

import (
	"bytes"
	"syscall"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

func TestResourceForkReadRangesAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset uint64
		length uint32
		getErr error
		want   string
		status NtStatus
	}{
		{name: "short", length: 2, want: "ab"},
		{name: "offset", offset: 2, length: 2, want: "cd"},
		{name: "tail", offset: 4, length: 10, want: "ef"},
		{name: "eof", offset: 6, length: 10, status: STATUS_END_OF_FILE},
		{name: "far-eof", offset: ^uint64(0), length: 2, status: STATUS_END_OF_FILE},
		{name: "zero-read", offset: 99, length: 0},
		{name: "io", length: 2, getErr: syscall.EIO, status: STATUS_IO_DEVICE_ERROR},
		{name: "missing", length: 2, getErr: missingXattrError, status: STATUS_OBJECT_NAME_NOT_FOUND},
		{name: "denied", length: 2, getErr: syscall.EACCES, status: STATUS_ACCESS_DENIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRangeXattrFS([]byte("abcdef"))
			f.getErr = tc.getErr
			tree, id, out := wireTree(t, f)
			open := tree.conn.serverCtx.getOpen(7)
			open.isEa = true
			open.eaKey = "AFP_Resource"
			req := &ReadRequest{FileId: id, Offset: tc.offset, Length: tc.length}
			if err := tree.readImpl(nil, requestBytes(req), id, open, 0); err != nil {
				t.Fatal(err)
			}
			select {
			case p := <-out:
				if got := NtStatus(PacketCodec(p).Status()); got != tc.status {
					t.Fatalf("status %v want %v", got, tc.status)
				}
				if tc.status == STATUS_SUCCESS {
					r := ReadResponseDecoder(p[64:])
					if r.IsInvalid() || !bytes.Equal(r.Data(), []byte(tc.want)) {
						t.Fatalf("data=%q want=%q", r.Data(), tc.want)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("no response")
			}
		})
	}
}
func TestAppleInformationDefaultRangedRead(t *testing.T) {
	f := newRangeXattrFS(nil)
	f.getErr = missingXattrError
	tree, id, out := wireTree(t, f)
	open := tree.conn.serverCtx.getOpen(7)
	open.isEa = true
	open.eaKey = "AFP_AfpInfo"
	req := &ReadRequest{FileId: id, Offset: 1, Length: 2}
	if err := tree.readImpl(nil, requestBytes(req), id, open, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-out:
		if got := NtStatus(PacketCodec(p).Status()); got != STATUS_SUCCESS {
			t.Fatal(got)
		}
		if got := ReadResponseDecoder(p[64:]).Data(); !bytes.Equal(got, []byte("FP")) {
			t.Fatalf("default information range %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no response")
	}
	f.getErr = syscall.EIO
	if err := tree.readImpl(nil, requestBytes(req), id, open, 0); err != nil {
		t.Fatal(err)
	}
	if got := wireStatus(t, out); got != STATUS_IO_DEVICE_ERROR {
		t.Fatalf("information stream hides error: %v", got)
	}
}
