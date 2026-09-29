package smb2

import (
	"context"
	"encoding/binary"
	"syscall"
	"testing"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

type contextLockFS struct {
	*faultFS
	gotContext context.Context
	err        error
}

func (f *contextLockFS) Lock(vfs.VfsHandle, []vfs.ByteRangeLock) error {
	panic("context-free lock used")
}
func (f *contextLockFS) LockContext(ctx context.Context, _ vfs.VfsHandle, _ []vfs.ByteRangeLock) error {
	f.gotContext = ctx
	return f.err
}
func lockPacket(id *FileId) []byte {
	b := make([]byte, 112)
	p := PacketCodec(b)
	p.SetProtocolId()
	p.SetStructureSize()
	p.SetCommand(SMB2_LOCK)
	binary.LittleEndian.PutUint16(b[64:66], 48)
	binary.LittleEndian.PutUint16(b[66:68], 1)
	id.Encode(b[72:88])
	binary.LittleEndian.PutUint64(b[96:104], 1)
	binary.LittleEndian.PutUint32(b[104:108], SMB2_LOCKFLAG_EXCLUSIVE_LOCK|SMB2_LOCKFLAG_FAIL_IMMEDIATELY)
	return b
}
func TestLockWireUsesCancelableNativeInterface(t *testing.T) {
	f := &contextLockFS{faultFS: &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}, err: syscall.EAGAIN}
	tree, id, out := wireTree(t, f)
	if err := tree.lock(nil, lockPacket(id)); err != nil {
		t.Fatal(err)
	}
	if got := wireStatus(t, out); got != STATUS_LOCK_NOT_GRANTED {
		t.Fatalf("status %v", got)
	}
	if f.gotContext != tree.conn.ctx {
		t.Fatal("connection cancellation not passed to native lock")
	}
	if len(tree.conn.serverCtx.getOpen(7).byteRangeLocks) != 0 {
		t.Fatal("failed native lock retained protocol reservation")
	}
}
func TestWriteThroughCreateOptionAndZeroWrite(t *testing.T) {
	for _, zero := range []bool{false, true} {
		f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}
		tree, id, out := wireTree(t, f)
		open := tree.conn.serverCtx.getOpen(7)
		open.createOptions = FILE_WRITE_THROUGH
		data := []byte("fixture")
		if zero {
			data = nil
		}
		if err := tree.writeImpl(nil, requestBytes(&WriteRequest{FileId: id, Data: data}), id, open, 0); err != nil {
			t.Fatal(err)
		}
		if status := wireStatus(t, out); status != STATUS_SUCCESS {
			t.Fatal(status)
		}
		if f.flushed != 1 {
			t.Fatal("create write-through ignored")
		}
	}
}
