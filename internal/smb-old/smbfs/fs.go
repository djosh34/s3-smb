// SPDX-License-Identifier: AGPL-3.0-only
// Package smbfs implements the SMB server's filesystem interface on JuiceFS.
package smbfs

import (
	"context"
	"errors"
	"io"
	"math"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	jvfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

// UID and GID are the JuiceFS identity of the one SMB account. serve gives it
// the root directory when it creates a dataset.
const UID uint32 = 65534
const GID uint32 = 65534

// FS holds the open SMB handles. The caller owns the JuiceFS filesystem and its
// session. Namespace operations take the write lock, so no other SMB operation
// can replace a checked path before it is used.
type FS struct {
	mu                sync.RWMutex
	jfs               *jfs.FileSystem
	meta              meta.Meta
	ctx               meta.Context
	readOnly, stopped bool
	handles           map[vfs.VfsHandle]*handle
}
type handle struct {
	file          *jfs.File
	path          string
	flags         int
	typ           uint8
	done          chan struct{}
	entries       []vfs.DirInfo
	cursor        int
	locks         []vfs.ByteRangeLock
	lockOwnerUsed bool
}

var nextHandle atomic.Uint64
var _ vfs.VFSFileSystem = (*FS)(nil)
var _ vfs.ByteRangeLocker = (*FS)(nil)

func New(filesystem *jfs.FileSystem, readOnly bool) (*FS, error) {
	if filesystem == nil {
		return nil, syscall.EINVAL
	}
	return &FS{jfs: filesystem, meta: filesystem.Meta(), ctx: meta.WrapWithoutCancel(context.Background(), 1, UID, []uint32{GID}), readOnly: readOnly, handles: make(map[vfs.VfsHandle]*handle)}, nil
}
func errno(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

// clientPath accepts share-relative paths only; the sole absolute spelling is
// the root itself. Do not clean away traversal before validating it.
func clientPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", syscall.EINVAL
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "/" || p == "" || p == "." {
		return "/", nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return "", syscall.EACCES
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return "", syscall.EACCES
		}
	}
	return path.Join("/", p), nil
}
func forbidden(ino meta.Ino, a *meta.Attr) bool {
	return jvfs.IsSpecialNode(ino) || a.Parent.IsTrash() || (ino != meta.RootInode && !ino.IsNormal())
}

// checkedPath resolves p through JuiceFS, symlinks included, and checks each
// resolved prefix against the forbidden inodes.
func (s *FS) checkedPath(p string, follow, missing bool) (string, error) {
	p, e := clientPath(p)
	if e != nil {
		return "", e
	}
	cur := ""
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, c := range parts {
		if c == "" {
			continue
		}
		if cur == "" && (c == meta.TrashName || jvfs.IsSpecialName(c)) {
			return "", syscall.EACCES
		}
		cur += "/" + c
		var st *jfs.FileStat
		var er syscall.Errno
		if i == len(parts)-1 && !follow {
			st, er = s.jfs.Lstat(s.ctx, cur)
		} else {
			st, er = s.jfs.Stat(s.ctx, cur)
		}
		if er == syscall.ENOENT && missing {
			return p, nil
		}
		if er != 0 {
			return "", er
		}
		if forbidden(st.Inode(), st.Attr()) {
			return "", syscall.EACCES
		}
	}
	return p, nil
}
func (s *FS) get(h vfs.VfsHandle) (*handle, error) {
	if s.stopped {
		return nil, syscall.EBADF
	}
	f := s.handles[h]
	if f == nil {
		return nil, syscall.EBADF
	}
	return f, nil
}
func (s *FS) writable(f *handle) error {
	if s.readOnly {
		return syscall.EROFS
	}
	if f == nil {
		return nil
	}
	var a meta.Attr
	if e := s.meta.GetAttr(s.ctx, f.file.Inode(), &a); e != 0 {
		return e
	}
	if forbidden(f.file.Inode(), &a) {
		return syscall.EACCES
	}
	// A hard-linked file has no single Attr.Parent. It stays writable while one
	// link is outside the trash. Do not write through a handle whose links are
	// all gone or in the trash, or the trashed copy changes.
	if a.Parent == 0 {
		for parent := range s.meta.GetParents(s.ctx, f.file.Inode()) {
			if !parent.IsTrash() {
				continue
			}
			if f.path == "" {
				return syscall.EACCES
			}
			p, e := s.checkedPath(strings.TrimPrefix(f.path, "/"), true, false)
			if e != nil {
				return syscall.EACCES
			}
			st, er := s.jfs.Stat(s.ctx, p)
			if er != 0 || st.Inode() != f.file.Inode() {
				return syscall.EACCES
			}
			break
		}
	}
	return nil
}
func (s *FS) add(f *jfs.File, p string, flags int) vfs.VfsHandle {
	h := vfs.VfsHandle(nextHandle.Add(1))
	info, _ := f.Stat() // taken at open; an inode keeps its file type
	s.handles[h] = &handle{file: f, path: p, flags: flags, typ: info.Sys().(*meta.Attr).Typ, done: make(chan struct{})}
	return h
}
func accessFlags(flags int) uint32 {
	switch flags & syscall.O_ACCMODE {
	case syscall.O_WRONLY:
		return jvfs.MODE_MASK_W
	case syscall.O_RDWR:
		return jvfs.MODE_MASK_R | jvfs.MODE_MASK_W
	default:
		return jvfs.MODE_MASK_R
	}
}
func (s *FS) Open(p string, flags, mode int) (vfs.VfsHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return 0, syscall.EBADF
	}
	if flags&syscall.O_ACCMODE == syscall.O_ACCMODE || flags&syscall.O_TRUNC != 0 && flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		return 0, syscall.EINVAL
	}
	writing := flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_CREAT|syscall.O_TRUNC) != 0
	if writing {
		if e := s.writable(nil); e != nil {
			return 0, e
		}
	}
	// The SMB server passes 0x200000 to open a reparse point itself.
	nofollow := flags&0x200000 != 0
	p, e := s.checkedPath(p, !nofollow, flags&syscall.O_CREAT != 0)
	if e != nil {
		return 0, e
	}
	st, er := s.jfs.Lstat(s.ctx, p)
	if er != 0 && er != syscall.ENOENT {
		return 0, er
	}
	if er == 0 && flags&(syscall.O_CREAT|syscall.O_EXCL) == syscall.O_CREAT|syscall.O_EXCL {
		return 0, syscall.EEXIST
	}
	if er == 0 && st.IsDir() && writing {
		return 0, syscall.EISDIR
	}
	var f *jfs.File
	if er == syscall.ENOENT && flags&syscall.O_CREAT != 0 {
		f, er = s.jfs.Create(s.ctx, p, uint16(mode)&0777, 0)
		if er != 0 {
			return 0, er
		}
		// Create returns a write-only file. Reopen it with the requested access.
		if er = f.Close(s.ctx); er != 0 {
			return 0, er
		}
	}
	if nofollow {
		f, er = s.jfs.Lopen(s.ctx, p, accessFlags(flags))
	} else {
		f, er = s.jfs.Open(s.ctx, p, accessFlags(flags))
	}
	if er != 0 {
		return 0, er
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close(s.ctx)
		return 0, statErr
	}
	if info.IsDir() {
		if er = s.meta.Access(s.ctx, f.Inode(), meta.MODE_MASK_R|meta.MODE_MASK_X, nil); er != 0 {
			_ = f.Close(s.ctx)
			return 0, er
		}
	}
	if flags&syscall.O_TRUNC != 0 {
		if er = f.Truncate(s.ctx, 0); er != 0 {
			_ = f.Close(s.ctx)
			return 0, er
		}
	}
	return s.add(f, p, flags), nil
}
func (s *FS) OpenDir(p string) (vfs.VfsHandle, error) {
	h, e := s.Open(p, syscall.O_RDONLY, 0)
	if e != nil {
		return 0, e
	}
	a, e := s.GetAttr(h)
	if e != nil {
		_ = s.Close(h)
		return 0, e
	}
	if a.GetFileType() != vfs.FileTypeDirectory {
		_ = s.Close(h)
		return 0, syscall.ENOTDIR
	}
	return h, nil
}
func (s *FS) close(h vfs.VfsHandle) error {
	f := s.handles[h]
	if f == nil {
		return syscall.EBADF
	}
	delete(s.handles, h)
	close(f.done)
	var unlock error
	if f.lockOwnerUsed {
		unlock = errno(s.meta.Setlk(s.ctx, f.file.Inode(), uint64(h), false, syscall.F_UNLCK, 0, math.MaxUint64, 1))
	}
	flush := errno(f.file.Fsync(s.ctx))
	return errors.Join(unlock, flush, errno(f.file.Close(s.ctx)))
}
func (s *FS) Close(h vfs.VfsHandle) error { s.mu.Lock(); defer s.mu.Unlock(); return s.close(h) }

// Shutdown flushes and closes every handle after the SMB server has drained its
// requests. Later calls fail and pending lock calls wake up.
func (s *FS) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	var result error
	for h := range s.handles {
		result = errors.Join(result, s.close(h))
	}
	return result
}
func (s *FS) Flush(h vfs.VfsHandle) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, e := s.get(h)
	if e != nil {
		return e
	}
	return errno(f.file.Fsync(s.ctx))
}
func (s *FS) FSync(h vfs.VfsHandle) error { return s.Flush(h) }
func (s *FS) Read(h vfs.VfsHandle, b []byte, off uint64, flags int) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, e := s.get(h)
	if e != nil {
		return 0, e
	}
	if off > math.MaxInt64 || uint64(len(b)) > math.MaxInt64-off {
		return 0, syscall.EINVAL
	}
	if f.flags&syscall.O_ACCMODE == syscall.O_WRONLY {
		return 0, syscall.EBADF
	}
	if e = dataFile(f); e != nil {
		return 0, e
	}
	if e = s.checkIO(h, f, off, len(b), false); e != nil {
		return 0, e
	}
	n, e := f.file.Pread(s.ctx, b, int64(off))
	if e == io.EOF && n > 0 {
		e = nil
	}
	return n, e
}
func (s *FS) Write(h vfs.VfsHandle, b []byte, off uint64, flags int) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, e := s.get(h)
	if e != nil {
		return 0, e
	}
	if e = s.writable(f); e != nil {
		return 0, e
	}
	if off > math.MaxInt64 || uint64(len(b)) > math.MaxInt64-off {
		return 0, syscall.EINVAL
	}
	if f.flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		return 0, syscall.EBADF
	}
	if e = dataFile(f); e != nil {
		return 0, e
	}
	if e = s.checkIO(h, f, off, len(b), true); e != nil {
		return 0, e
	}
	n, er := f.file.Pwrite(s.ctx, b, int64(off))
	if er != 0 {
		return n, er
	}
	// The SMB server handles WRITE_THROUGH itself. A handle opened with O_SYNC
	// syncs here.
	if f.flags&syscall.O_SYNC != 0 {
		return n, errno(f.file.Fsync(s.ctx))
	}
	return n, nil
}
func (s *FS) Truncate(h vfs.VfsHandle, size uint64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, e := s.get(h)
	if e != nil {
		return e
	}
	if e = s.writable(f); e != nil {
		return e
	}
	if size > math.MaxInt64 {
		return syscall.EINVAL
	}
	if f.flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		return syscall.EBADF
	}
	if e = dataFile(f); e != nil {
		return e
	}
	return errno(f.file.Truncate(s.ctx, size))
}
func dataFile(f *handle) error {
	if f.typ == meta.TypeDirectory {
		return syscall.EISDIR
	}
	if f.typ != meta.TypeFile {
		return syscall.ENOTSUP
	}
	return nil
}
