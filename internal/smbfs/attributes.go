// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"math"
	"strings"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func attributes(ino meta.Ino, a *meta.Attr) *vfs.Attributes {
	typ := vfs.FileTypeOther
	switch a.Typ {
	case meta.TypeFile:
		typ = vfs.FileTypeRegularFile
	case meta.TypeDirectory:
		typ = vfs.FileTypeDirectory
	case meta.TypeSymlink:
		typ = vfs.FileTypeSymlink
	case meta.TypeFIFO:
		typ = vfs.FileTypeFIFO
	case meta.TypeSocket:
		typ = vfs.FileTypeSocket
	case meta.TypeBlockDev:
		typ = vfs.FileTypeBlockDevice
	case meta.TypeCharDev:
		typ = vfs.FileTypeCharacterDevice
	}
	return new(vfs.Attributes).SetFileHandle(vfs.VfsNode(ino)).SetInodeNumber(uint64(ino)).SetFileType(typ).
		SetChangeID(uint64(a.Ctime)*1e9 + uint64(a.Ctimensec)).SetDeviceNumber(uint64(a.Rdev)).
		SetLinkCount(a.Nlink).SetPermissions(vfs.NewPermissionsFromMode(uint32(a.Mode))).SetUnixMode(uint32(a.Mode)).
		SetUID(a.Uid).SetGID(a.Gid).SetSizeBytes(a.Length).SetDiskSizeBytes((a.Length + 4095) / 4096 * 4096).
		SetAccessTime(time.Unix(a.Atime, int64(a.Atimensec))).SetLastDataModificationTime(time.Unix(a.Mtime, int64(a.Mtimensec))).
		SetLastStatusChangeTime(time.Unix(a.Ctime, int64(a.Ctimensec)))
}
func (s *FS) attr(f *handle) (*vfs.Attributes, error) {
	// Native File.Stat is an open-time snapshot. Commit buffered size changes
	// before querying metadata; do not synthesize a second inode attribute cache.
	for _, other := range s.handles {
		if other.file.Inode() == f.file.Inode() {
			if e := other.file.Flush(s.ctx); e != 0 {
				return nil, e
			}
		}
	}
	var a meta.Attr
	if e := s.meta.GetAttr(s.ctx, f.file.Inode(), &a); e != 0 {
		return nil, e
	}
	return attributes(f.file.Inode(), &a), nil
}
func (s *FS) GetAttr(h vfs.VfsHandle) (*vfs.Attributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return nil, e
	}
	return s.attr(f)
}
func (s *FS) SetAttr(h vfs.VfsHandle, a *vfs.Attributes) (*vfs.Attributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return nil, e
	}
	if e = s.writable(f); e != nil {
		return nil, e
	}
	if a == nil {
		return nil, syscall.EINVAL
	}
	var n meta.Attr
	var mask uint16
	if v, ok := a.GetUnixMode(); ok {
		n.Mode = uint16(v) & 0777
		mask |= meta.SetAttrMode
	} else if v, ok := a.GetPermissions(); ok {
		n.Mode = uint16(v.ToMode())
		mask |= meta.SetAttrMode
	}
	if v, ok := a.GetUID(); ok {
		n.Uid = v
		mask |= meta.SetAttrUID
	}
	if v, ok := a.GetGID(); ok {
		n.Gid = v
		mask |= meta.SetAttrGID
	}
	if v, ok := a.GetAccessTime(); ok {
		n.Atime = v.Unix()
		n.Atimensec = uint32(v.Nanosecond())
		mask |= meta.SetAttrAtime
	}
	if v, ok := a.GetLastDataModificationTime(); ok {
		n.Mtime = v.Unix()
		n.Mtimensec = uint32(v.Nanosecond())
		mask |= meta.SetAttrMtime
	}
	// JuiceFS has no independent birth time. Do not reinterpret it as ctime.
	if size, ok := a.GetSizeBytes(); ok {
		if e := dataFile(f); e != nil {
			return nil, e
		}
		if size > math.MaxInt64 {
			return nil, syscall.EINVAL
		}
		if f.flags&syscall.O_ACCMODE == syscall.O_RDONLY {
			return nil, syscall.EBADF
		}
		if er := f.file.Truncate(s.ctx, size); er != 0 {
			return nil, er
		}
	}
	if mask != 0 {
		if er := s.meta.SetAttr(s.ctx, f.file.Inode(), mask, 0, &n); er != 0 {
			return nil, er
		}
		s.native.InvalidateAttr(f.file.Inode())
	}
	return s.attr(f)
}
func (s *FS) StatFS(h vfs.VfsHandle) (*vfs.FSAttributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h != 0 {
		if _, e := s.get(h); e != nil {
			return nil, e
		}
	} else if s.stopped {
		return nil, syscall.EBADF
	}
	var total, avail, used, free uint64
	if e := s.meta.StatFS(s.ctx, meta.RootInode, &total, &avail, &used, &free); e != 0 {
		return nil, e
	}
	return new(vfs.FSAttributes).SetBlockSize(4096).SetIOSize(1 << 20).SetBlocks(total / 4096).SetFreeBlocks(avail / 4096).SetAvailableBlocks(avail / 4096).SetFiles(used + free).SetFreeFiles(free), nil
}
func (s *FS) Listxattr(h vfs.VfsHandle) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return nil, e
	}
	var b []byte
	if er := s.meta.ListXattr(s.ctx, f.file.Inode(), &b); er != 0 {
		return nil, er
	}
	if len(b) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), nil
}
func (s *FS) Getxattr(h vfs.VfsHandle, key string, b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return 0, e
	}
	var value []byte
	if er := s.meta.GetXattr(s.ctx, f.file.Inode(), key, &value); er != 0 {
		return 0, er
	}
	if b == nil {
		return len(value), nil
	}
	if len(b) < len(value) {
		return 0, syscall.ERANGE
	}
	return copy(b, value), nil
}
func (s *FS) Setxattr(h vfs.VfsHandle, key string, b []byte) error {
	// The direct metadata API does not apply the native VFS xattr bound.
	// Use the same bound as the SMB resource-fork range allocator.
	if len(b) > vfs.MaxXattrSize {
		return syscall.E2BIG
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return e
	}
	if e = s.writable(f); e != nil {
		return e
	}
	if er := s.meta.Access(s.ctx, f.file.Inode(), meta.MODE_MASK_W, nil); er != 0 {
		return er
	}
	// Empty attributes exist and differ from missing ones. SQLite's native
	// schema requires a non-NULL blob; a nil Go slice would bind SQL NULL.
	if b == nil {
		b = []byte{}
	}
	return errno(s.meta.SetXattr(s.ctx, f.file.Inode(), key, b, 0))
}
func (s *FS) Removexattr(h vfs.VfsHandle, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return e
	}
	if e = s.writable(f); e != nil {
		return e
	}
	if er := s.meta.Access(s.ctx, f.file.Inode(), meta.MODE_MASK_W, nil); er != 0 {
		return er
	}
	return errno(s.meta.RemoveXattr(s.ctx, f.file.Inode(), key))
}
