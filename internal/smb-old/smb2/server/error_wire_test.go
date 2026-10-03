package smb2

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
	client "github.com/hirochachacha/go-smb2"
)

// These are real TCP SMB requests against a fault-injecting native VFS, not an
// S3 integration test. The external client decodes and checks the wire status.
func TestNativeErrorsOverTCP(t *testing.T) {
	for _, op := range []string{"write", "flush", "close", "resource-fork"} {
		t.Run(op, func(t *testing.T) {
			f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
			switch op {
			case "write":
				f.writeErr = syscall.ENOSPC
			case "flush":
				f.flushErr = syscall.EIO
			case "close":
				f.closeErr = syscall.EIO
			case "resource-fork":
				f.xattrErr = syscall.EROFS
			}
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			s := NewServer(&ServerConfig{Xatrrs: true}, &NTLMAuthenticator{UserPassword: map[string]string{"backup": ""}}, map[string]vfs.VFSFileSystem{"backup": f})
			go func() { _ = s.ServeListener(l) }()
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				e := s.ShutdownContext(ctx)
				if errors.Is(e, context.DeadlineExceeded) {
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
			name := "fixture"
			if op == "resource-fork" {
				name += ":AFP_Resource"
			}
			file, e := share.OpenFile(name, os.O_RDWR, 0600)
			if e != nil {
				t.Fatal(e)
			}
			switch op {
			case "write", "resource-fork":
				_, e = file.Write([]byte("fixture"))
			case "flush":
				e = file.Sync()
			case "close":
				e = file.Close()
			}
			var response *client.ResponseError
			if !errors.As(e, &response) {
				t.Fatalf("expected protocol error, got %v", e)
			}
			want := STATUS_IO_DEVICE_ERROR
			if op == "write" {
				want = STATUS_DISK_FULL
			}
			if op == "resource-fork" {
				want = STATUS_MEDIA_WRITE_PROTECTED
			}
			if response.Code != uint32(want) {
				t.Fatalf("status %#x, want %#x", response.Code, want)
			}
		})
	}
}
