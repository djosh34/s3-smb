package smb2

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

type rangeXattrFS struct {
	*faultFS
	mu                      sync.Mutex
	value                   []byte
	getErr, readErr, setErr error
	probeSize               int
	probes, reads, sets     int
	next                    atomic.Uint64
}

func newRangeXattrFS(value []byte) *rangeXattrFS {
	return &rangeXattrFS{faultFS: &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}, value: append([]byte{}, value...), probeSize: -1}
}
func (f *rangeXattrFS) Open(string, int, int) (vfs.VfsHandle, error) {
	return vfs.VfsHandle(f.next.Add(1)), nil
}
func (f *rangeXattrFS) Flush(vfs.VfsHandle) error { return nil }
func (f *rangeXattrFS) Close(vfs.VfsHandle) error { return nil }
func (f *rangeXattrFS) Getxattr(_ vfs.VfsHandle, _ string, b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b == nil {
		f.probes++
	} else {
		f.reads++
	}
	if f.getErr != nil {
		return 0, f.getErr
	}
	if b == nil {
		if f.probeSize >= 0 {
			return f.probeSize, nil
		}
		return len(f.value), nil
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	if len(b) < len(f.value) {
		return 0, syscall.ERANGE
	}
	return copy(b, f.value), nil
}
func (f *rangeXattrFS) Setxattr(_ vfs.VfsHandle, _ string, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.setErr != nil {
		return f.setErr
	}
	f.value = append([]byte{}, b...)
	if errors.Is(f.getErr, missingXattrError) {
		f.getErr = nil
	}
	return nil
}
func (f *rangeXattrFS) snapshot() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte{}, f.value...)
}

func TestResourceForkRangedWriteWire(t *testing.T) {
	for _, tc := range []struct {
		name       string
		old        string
		offset     uint64
		data, want []byte
	}{
		{"middle-preserves-tail", "abcdef", 2, []byte("XY"), []byte("abXYef")},
		{"prefix-preserves-length", "abcdef", 0, []byte("X"), []byte("Xbcdef")},
		{"append", "abc", 3, []byte("XYZ"), []byte("abcXYZ")},
		{"hole", "abc", 5, []byte("X"), []byte{'a', 'b', 'c', 0, 0, 'X'}},
		{"empty-no-truncate", "abcdef", 0, nil, []byte("abcdef")},
		{"empty-no-hole", "abcdef", 99, nil, []byte("abcdef")},
		{"empty-stream", "", 0, nil, []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRangeXattrFS([]byte(tc.old))
			tree, id, out := wireTree(t, f)
			open := tree.conn.serverCtx.getOpen(7)
			open.isEa = true
			open.eaKey = "AFP_Resource"
			req := &WriteRequest{FileId: id, Offset: tc.offset, Data: tc.data}
			if err := tree.writeImpl(nil, requestBytes(req), id, open, 0); err != nil {
				t.Fatal(err)
			}
			if got := wireStatus(t, out); got != STATUS_SUCCESS {
				t.Fatal(got)
			}
			if got := f.snapshot(); !bytes.Equal(got, tc.want) {
				t.Fatalf("stream = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResourceForkOffsetOverTCP(t *testing.T) {
	f := newRangeXattrFS([]byte("abcdef"))
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	s := NewServer(&ServerConfig{Xatrrs: true}, &NTLMAuthenticator{UserPassword: map[string]string{"backup": ""}}, map[string]vfs.VFSFileSystem{"backup": f})
	go func() { _ = s.ServeListener(l) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if e := s.ShutdownContext(ctx); e != nil {
			t.Fatal(e)
		}
	}()
	session, c, e := dialWire(l, "backup", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	share, e := session.Mount("backup")
	if e != nil {
		t.Fatal(e)
	}
	file, e := share.OpenFile("fixture:AFP_Resource", os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if n, e := file.WriteAt([]byte("XY"), 2); e != nil || n != 2 {
		t.Fatalf("write = %d, %v", n, e)
	}
	got := make([]byte, 6)
	if n, e := file.ReadAt(got, 0); e != nil || n != 6 {
		t.Fatalf("read = %d, %v", n, e)
	}
	if !bytes.Equal(got, []byte("abXYef")) {
		t.Fatalf("wire read %q, want abXYef", got)
	}
	if e = file.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestResourceForkRangeErrors(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		offset                  uint64
		data                    []byte
		getErr, readErr, setErr error
		probeSize               int
		want                    NtStatus
	}{
		{name: "overflow", offset: ^uint64(0), data: []byte("x"), probeSize: -1, want: STATUS_EA_TOO_LARGE},
		{name: "past-native-max", offset: vfs.MaxXattrSize, data: []byte("x"), probeSize: -1, want: STATUS_EA_TOO_LARGE},
		{name: "oversized-native-value", data: []byte("x"), probeSize: vfs.MaxXattrSize + 1, want: STATUS_EA_TOO_LARGE},
		{name: "readonly", data: []byte("x"), setErr: syscall.EROFS, probeSize: -1, want: STATUS_MEDIA_WRITE_PROTECTED},
		{name: "empty-readonly", setErr: syscall.EROFS, probeSize: -1, want: STATUS_MEDIA_WRITE_PROTECTED},
		{name: "probe-io", data: []byte("x"), getErr: syscall.EIO, probeSize: -1, want: STATUS_IO_DEVICE_ERROR},
		{name: "set-io", data: []byte("x"), setErr: syscall.EIO, probeSize: -1, want: STATUS_IO_DEVICE_ERROR},
		{name: "read-io", data: []byte("x"), readErr: syscall.EIO, probeSize: -1, want: STATUS_IO_DEVICE_ERROR},
		{name: "probe-growth", data: []byte("x"), probeSize: 4, want: STATUS_BUFFER_TOO_SMALL},
		{name: "probe-shrink", data: []byte("x"), probeSize: 8, want: STATUS_IO_DEVICE_ERROR},
		{name: "oversized-data", data: make([]byte, vfs.MaxXattrSize+1), probeSize: -1, want: STATUS_EA_TOO_LARGE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRangeXattrFS([]byte("abcdef"))
			f.getErr = tc.getErr
			f.readErr = tc.readErr
			f.setErr = tc.setErr
			f.probeSize = tc.probeSize
			tree, id, out := wireTree(t, f)
			open := tree.conn.serverCtx.getOpen(7)
			open.isEa = true
			if e := tree.writeImpl(nil, requestBytes(&WriteRequest{FileId: id, Offset: tc.offset, Data: tc.data}), id, open, 0); e != nil {
				t.Fatal(e)
			}
			if got := wireStatus(t, out); got != tc.want {
				t.Fatalf("status %v, want %v", got, tc.want)
			}
			if !bytes.Equal(f.snapshot(), []byte("abcdef")) {
				t.Fatal("failed write changed old value")
			}
			if tc.want == STATUS_EA_TOO_LARGE && f.reads != 0 {
				t.Fatal("oversized value allocated/read before bound")
			}
			if errors.Is(tc.getErr, syscall.EIO) && f.sets != 0 {
				t.Fatal("read failure overwrote xattr")
			}
		})
	}
}
