package smb2

import (
	"errors"
	"syscall"

	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

// writeXattrRange implements the named stream over the native xattr, without a
// second store. All SMB xattr mutations share the server mutex, including whole
// setters/removal and create-time truncation, across sessions and handles.
func (t *fileTree) writeXattrRange(h vfs.VfsHandle, key string, data []byte, offset uint64) error {
	if offset > vfs.MaxXattrSize || uint64(len(data)) > uint64(vfs.MaxXattrSize)-offset {
		return syscall.E2BIG
	}
	t.conn.serverCtx.xattrMu.Lock()
	defer t.conn.serverCtx.xattrMu.Unlock()
	size, err := t.fs.Getxattr(h, key, nil)
	if errors.Is(err, missingXattrError) {
		size, err = 0, nil
	}
	if err != nil {
		return err
	}
	if size < 0 {
		return syscall.EIO
	}
	if size > vfs.MaxXattrSize {
		return syscall.E2BIG
	}
	length := size
	if len(data) > 0 && int(offset)+len(data) > length {
		length = int(offset) + len(data)
	}
	value := make([]byte, length)
	if size > 0 {
		n, err := t.fs.Getxattr(h, key, value)
		if err != nil {
			return err
		}
		if n != size {
			return syscall.EIO
		}
	}
	if len(data) > 0 {
		copy(value[int(offset):], data)
	}
	// An empty write preserves the old length/data. Passing the unchanged value
	// through the native setter also preserves read-only/access error behavior.
	return t.fs.Setxattr(h, key, value)
}

// resizeXattr preserves the existing prefix when truncating or extending a
// named stream. Check native bounds before allocating any requested length.
func (t *fileTree) resizeXattr(h vfs.VfsHandle, key string, length int64) error {
	if length < 0 {
		return syscall.EINVAL
	}
	if length > vfs.MaxXattrSize {
		return syscall.E2BIG
	}
	t.conn.serverCtx.xattrMu.Lock()
	defer t.conn.serverCtx.xattrMu.Unlock()
	size, err := t.fs.Getxattr(h, key, nil)
	if errors.Is(err, missingXattrError) {
		size, err = 0, nil
	}
	if err != nil {
		return err
	}
	if size < 0 {
		return syscall.EIO
	}
	if size > vfs.MaxXattrSize {
		return syscall.E2BIG
	}
	old := make([]byte, size)
	if size > 0 {
		n, err := t.fs.Getxattr(h, key, old)
		if err != nil {
			return err
		}
		if n != size {
			return syscall.EIO
		}
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
