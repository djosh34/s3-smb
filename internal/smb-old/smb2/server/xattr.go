package smb2

import (
	"errors"
	"syscall"

	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

// loadXattr reads one bounded native value. The caller holds the server xattr
// mutex so size/value reads and read-modify-write operations stay coherent.
func (t *fileTree) loadXattr(h vfs.VfsHandle, key string) ([]byte, error) {
	size, err := t.fs.Getxattr(h, key, nil)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, syscall.EIO
	}
	if size > vfs.MaxXattrSize {
		return nil, syscall.E2BIG
	}
	value := make([]byte, size)
	if size > 0 {
		n, err := t.fs.Getxattr(h, key, value)
		if err != nil {
			return nil, err
		}
		if n != size {
			return nil, syscall.EIO
		}
	}
	return value, nil
}
func (t *fileTree) readXattr(h vfs.VfsHandle, key string) ([]byte, error) {
	t.conn.serverCtx.xattrMu.Lock()
	defer t.conn.serverCtx.xattrMu.Unlock()
	return t.loadXattr(h, key)
}

// writeXattrRange keeps the named stream in the native xattr, without a second
// store. All SMB mutations share the mutex, including creation and resizing.
func (t *fileTree) writeXattrRange(h vfs.VfsHandle, key string, data []byte, offset uint64) error {
	if offset > vfs.MaxXattrSize || uint64(len(data)) > uint64(vfs.MaxXattrSize)-offset {
		return syscall.E2BIG
	}
	t.conn.serverCtx.xattrMu.Lock()
	defer t.conn.serverCtx.xattrMu.Unlock()
	old, err := t.loadXattr(h, key)
	if errors.Is(err, missingXattrError) {
		old, err = nil, nil
	}
	if err != nil {
		return err
	}
	length := len(old)
	if len(data) > 0 && int(offset)+len(data) > length {
		length = int(offset) + len(data)
	}
	value := make([]byte, length)
	copy(value, old)
	if len(data) > 0 {
		copy(value[int(offset):], data)
	}
	// Empty writes preserve length/data, but still enforce native write access.
	return t.fs.Setxattr(h, key, value)
}

// resizeXattr preserves the prefix and checks bounds before allocating length.
func (t *fileTree) resizeXattr(h vfs.VfsHandle, key string, length int64) error {
	if length < 0 {
		return syscall.EINVAL
	}
	if length > vfs.MaxXattrSize {
		return syscall.E2BIG
	}
	t.conn.serverCtx.xattrMu.Lock()
	defer t.conn.serverCtx.xattrMu.Unlock()
	old, err := t.loadXattr(h, key)
	if errors.Is(err, missingXattrError) {
		old, err = nil, nil
	}
	if err != nil {
		return err
	}
	value := make([]byte, int(length))
	copy(value, old)
	return t.fs.Setxattr(h, key, value)
}
func (t *fileTree) setXattr(h vfs.VfsHandle, key string, value []byte) error {
	if len(value) > vfs.MaxXattrSize {
		return syscall.E2BIG
	}
	t.conn.serverCtx.xattrMu.Lock()
	defer t.conn.serverCtx.xattrMu.Unlock()
	return t.fs.Setxattr(h, key, value)
}
func (t *fileTree) removeXattr(h vfs.VfsHandle, key string) error {
	t.conn.serverCtx.xattrMu.Lock()
	defer t.conn.serverCtx.xattrMu.Unlock()
	return t.fs.Removexattr(h, key)
}
