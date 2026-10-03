package smb2

import (
	"context"
	"fmt"
	"syscall"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

type faultFS struct {
	*deleteDispositionFS
	writeErr, flushErr, closeErr, xattrErr error
	flushed, closed, written               int
}

func (f *faultFS) Write(_ vfs.VfsHandle, b []byte, _ uint64, _ int) (int, error) {
	f.written++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(b), nil
}
func (f *faultFS) Flush(vfs.VfsHandle) error                    { f.flushed++; return f.flushErr }
func (f *faultFS) Close(vfs.VfsHandle) error                    { f.closed++; return f.closeErr }
func (f *faultFS) Setxattr(vfs.VfsHandle, string, []byte) error { return f.xattrErr }

func wireTree(t *testing.T, fs vfs.VFSFileSystem) (*fileTree, *FileId, <-chan []byte) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &conn{ctx: ctx, cancel: cancel, account: openAccount(16), write: make(chan []byte), werr: make(chan error), serverState: STATE_SESSION_ACTIVE}
	c.serverCtx = NewServer(&ServerConfig{}, nil, nil)
	s := &session{conn: c, sessionId: 1}
	tree := &fileTree{treeConn: treeConn{session: s, treeId: 1}, fs: fs}
	id := &FileId{}
	id.SetHandleId(7)
	id.SetNodeId(42)
	c.serverCtx.addOpen(&Open{fileId: 7, durableFileId: 42, tree: &tree.treeConn, session: s})
	out := make(chan []byte, 8)
	go func() {
		for {
			select {
			case p := <-c.write:
				out <- p
				c.werr <- nil
			case <-ctx.Done():
				return
			}
		}
	}()
	return tree, id, out
}
func requestBytes(p Packet) []byte { b := make([]byte, p.Size()); p.Encode(b); return b }
func wireStatus(t *testing.T, out <-chan []byte) NtStatus {
	t.Helper()
	select {
	case p := <-out:
		return NtStatus(PacketCodec(p).Status())
	case <-time.After(time.Second):
		t.Fatal("no wire response")
		return 0
	}
}

func TestDurabilityWireErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want NtStatus
	}{
		{"io", syscall.EIO, STATUS_IO_DEVICE_ERROR}, {"full", syscall.ENOSPC, STATUS_DISK_FULL}, {"readonly", syscall.EROFS, STATUS_MEDIA_WRITE_PROTECTED}, {"denied", syscall.EACCES, STATUS_ACCESS_DENIED}, {"missing", syscall.ENOENT, STATUS_OBJECT_NAME_NOT_FOUND}, {"handle", syscall.EBADF, STATUS_INVALID_HANDLE},
	} {
		for _, op := range []string{"flush", "close", "write", "xattr", "write-through", "xattr-through"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
				tree, id, out := wireTree(t, f)
				open := tree.conn.serverCtx.getOpen(id.HandleId())
				var err error
				switch op {
				case "flush":
					f.flushErr = fmt.Errorf("backend: %w", tc.err)
					err = tree.flush(nil, requestBytes(&FlushRequest{FileId: id}))
				case "close":
					f.closeErr = tc.err
					err = tree.close(nil, requestBytes(&CloseRequest{FileId: id}))
				default:
					req := &WriteRequest{FileId: id, Data: []byte("fixture")}
					switch op {
					case "write":
						f.writeErr = tc.err
					case "xattr":
						open.isEa = true
						f.xattrErr = tc.err
					case "write-through":
						req.Flags = SMB2_WRITEFLAG_WRITE_THROUGH
						f.flushErr = tc.err
					case "xattr-through":
						open.isEa = true
						req.Flags = SMB2_WRITEFLAG_WRITE_THROUGH
						f.flushErr = tc.err
					}
					err = tree.writeImpl(nil, requestBytes(req), id, open, 0)
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := wireStatus(t, out); got != tc.want {
					t.Fatalf("wire status %v, want %v", got, tc.want)
				}
				if op == "close" && (f.closed != 1 || tree.conn.serverCtx.getOpen(7) != nil) {
					t.Fatal("failed close did not release handle")
				}
			})
		}
	}
}

func TestFlushRejectsForeignHandle(t *testing.T) {
	f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
	tree, id, out := wireTree(t, f)
	tree.conn.serverCtx.getOpen(7).session = &session{}
	if err := tree.flush(nil, requestBytes(&FlushRequest{FileId: id})); err != nil {
		t.Fatal(err)
	}
	if got := wireStatus(t, out); got != STATUS_INVALID_HANDLE {
		t.Fatalf("status %v", got)
	}
	if f.flushed != 0 {
		t.Fatal("foreign handle reached backend")
	}
}
