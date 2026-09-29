package smb2

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func (f *faultFS) Open(string, int, int) (vfs.VfsHandle, error)          { return 7, nil }
func (f *faultFS) Lookup(vfs.VfsHandle, string) (*vfs.Attributes, error) { return &f.attrs, nil }
func (f *faultFS) Getxattr(vfs.VfsHandle, string, []byte) (int, error)   { return 0, nil }

func TestDisconnectFlushesAndClosesHandles(t *testing.T) {
	f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	s := NewServer(&ServerConfig{}, &NTLMAuthenticator{UserPassword: map[string]string{"backup": ""}}, map[string]vfs.VFSFileSystem{"backup": f})
	go func() { _ = s.ServeListener(l) }()
	session, c, e := dialWire(l, "backup", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	share, e := session.Mount("backup")
	if e != nil {
		t.Fatal(e)
	}
	file, e := share.OpenFile("fixture", 2, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = file.Write([]byte("fixture")); e != nil {
		t.Fatal(e)
	}
	c.Close() // abrupt disconnect, without CLOSE/TREE_DISCONNECT/LOGOFF
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = s.ShutdownContext(ctx); e != nil {
		t.Fatal(e)
	}
	if f.flushed != 1 || f.closed != 1 {
		t.Fatalf("flush=%d close=%d", f.flushed, f.closed)
	}
	if len(s.opens) != 0 {
		t.Fatal("disconnected handles retained")
	}
}

func TestCleanupReportsFailureButReleasesAllHandles(t *testing.T) {
	f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile), flushErr: syscall.EIO}
	tree, _, _ := wireTree(t, f)
	tree.conn.treeMapById = map[uint32]treeOps{tree.treeId: tree}
	if e := tree.conn.closeTreeHandles(nil); e == nil {
		t.Fatal("flush error lost")
	}
	if f.closed != 1 || len(tree.conn.serverCtx.opens) != 0 {
		t.Fatal("failed cleanup leaked handle")
	}
	if e := tree.conn.closeTreeHandles(nil); e != nil {
		t.Fatal(e)
	}
	if f.closed != 1 {
		t.Fatal("double close")
	}
}

func TestShutdownBeforeServeDoesNotReopen(t *testing.T) {
	s := NewServer(&ServerConfig{}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := s.ShutdownContext(ctx); e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ServeListener(l); e == nil {
		t.Fatal("shutdown server started")
	}
}
