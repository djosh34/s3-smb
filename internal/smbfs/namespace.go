// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"path"
	"strings"
	"syscall"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	jvfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func (s *FS) Lookup(h vfs.VfsHandle, name string) (*vfs.Attributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, syscall.EBADF
	}
	p, e := clientPath(name)
	if e != nil {
		return nil, e
	}
	if h != 0 {
		f, er := s.get(h)
		if er != nil {
			return nil, er
		}
		if er = s.pathIdentity(f); er != nil {
			return nil, er
		}
		p = path.Join(f.path, p)
	}
	p, e = s.checkedPath(strings.TrimPrefix(p, "/"), false, false)
	if e != nil {
		return nil, e
	}
	st, er := s.native.Lstat(s.ctx, p)
	if er != 0 {
		return nil, er
	}
	return attributes(st.Inode(), st.Attr()), nil
}
func (s *FS) Mkdir(p string, mode int) (*vfs.Attributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, syscall.EBADF
	}
	if e := s.writable(nil); e != nil {
		return nil, e
	}
	p, e := s.checkedPath(p, false, true)
	if e != nil {
		return nil, e
	}
	if er := s.native.Mkdir(s.ctx, p, uint16(mode)&0777, 0); er != 0 {
		return nil, er
	}
	st, er := s.native.Lstat(s.ctx, p)
	if er != 0 {
		return nil, er
	}
	return attributes(st.Inode(), st.Attr()), nil
}
func (s *FS) ReadDir(h vfs.VfsHandle, flags, max int) ([]vfs.DirInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return nil, e
	}
	if flags != vfs.ReadDirContinue && flags != vfs.ReadDirRestart {
		return nil, syscall.EINVAL
	}
	if flags == vfs.ReadDirRestart {
		f.entries = nil
		f.cursor = 0
	}
	if f.entries == nil {
		var entries []*meta.Entry
		if er := s.meta.Readdir(s.ctx, f.file.Inode(), 1, &entries); er != 0 {
			return nil, er
		}
		f.entries = make([]vfs.DirInfo, 0, len(entries))
		for _, entry := range entries {
			name := string(entry.Name)
			if name == "." || name == ".." || forbidden(entry.Inode, entry.Attr) || (f.file.Inode() == meta.RootInode && name == meta.TrashName) {
				continue
			}
			f.entries = append(f.entries, vfs.DirInfo{Name: name, Attributes: *attributes(entry.Inode, entry.Attr)})
		}
	}
	end := len(f.entries)
	if max > 0 && max < end-f.cursor {
		end = f.cursor + max
	}
	result := append([]vfs.DirInfo(nil), f.entries[f.cursor:end]...)
	f.cursor = end
	return result, nil
}

// A handle is inode-bound. Never let a stale name delete/rename a replacement.
func (s *FS) pathIdentity(f *handle) error {
	if f.path == "" {
		return syscall.ENOENT
	}
	if _, e := s.checkedPath(strings.TrimPrefix(f.path, "/"), false, false); e != nil {
		return e
	}
	st, er := s.native.Lstat(s.ctx, f.path)
	if er != 0 {
		return er
	}
	if st.Inode() != f.file.Inode() {
		return syscall.ESTALE
	}
	return nil
}
func (s *FS) Unlink(h vfs.VfsHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return e
	}
	if e = s.writable(f); e != nil {
		return e
	}
	if e = s.pathIdentity(f); e != nil {
		return e
	}
	if f.path == "/" {
		return syscall.EACCES
	}
	p := f.path
	if er := s.native.Delete(s.ctx, p); er != 0 {
		return er
	}
	for _, other := range s.handles {
		if other.path == p {
			other.path = ""
		}
	}
	return nil
}
func (s *FS) Rename(h vfs.VfsHandle, to string, flags int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return e
	}
	if e = s.writable(f); e != nil {
		return e
	}
	if e = s.pathIdentity(f); e != nil {
		return e
	}
	if flags != 0 && flags != 1 {
		return syscall.EINVAL
	}
	target, e := s.checkedPath(to, false, true)
	if e != nil {
		return e
	}
	if f.path == "/" || target == "/" {
		return syscall.EACCES
	}
	var nativeFlags uint32
	if flags == 0 {
		nativeFlags = meta.RenameNoReplace
	}
	old := f.path
	if er := s.native.Rename(s.ctx, old, target, nativeFlags); er != 0 {
		return er
	}
	for _, other := range s.handles {
		switch {
		case other.path == old:
			other.path = target
		case strings.HasPrefix(other.path, old+"/"):
			other.path = target + strings.TrimPrefix(other.path, old)
		case other.path == target:
			other.path = ""
		}
	}
	return nil
}
func (s *FS) Readlink(h vfs.VfsHandle) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return "", e
	}
	var b []byte
	if er := s.meta.ReadLink(s.ctx, f.file.Inode(), &b); er != 0 {
		return "", er
	}
	return string(b), nil
}

// relativeTarget validates the target at its containing directory without
// changing the stored relative spelling (needed when directories are moved).
func relativeTarget(parent, target string) (string, error) {
	target = strings.ReplaceAll(target, "\\", "/")
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\x00:") {
		return "", syscall.EACCES
	}
	parts := strings.Split(strings.Trim(parent, "/"), "/")
	if parent == "/" {
		parts = nil
	}
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if len(parts) == 0 {
				return "", syscall.EACCES
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "/"), nil
}
func (s *FS) Symlink(h vfs.VfsHandle, target string, flags int) (*vfs.Attributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.get(h)
	if e != nil {
		return nil, e
	}
	if e = s.writable(f); e != nil {
		return nil, e
	}
	if e = s.pathIdentity(f); e != nil {
		return nil, e
	}
	if f.path == "/" || len(f.locks) > 0 {
		return nil, syscall.EACCES
	}
	resolved, e := relativeTarget(path.Dir(f.path), target)
	if e != nil {
		return nil, e
	}
	if _, e = s.checkedPath(resolved, true, true); e != nil {
		return nil, e
	}
	if er := f.file.Fsync(s.ctx); er != 0 {
		return nil, er
	}
	var a meta.Attr
	if er := s.meta.GetAttr(s.ctx, f.file.Inode(), &a); er != 0 {
		return nil, er
	}
	if a.Typ != meta.TypeFile || a.Length != 0 {
		return nil, syscall.EINVAL
	}
	p := f.path
	if er := s.native.Delete(s.ctx, p); er != 0 {
		return nil, er
	}
	for _, other := range s.handles {
		if other.path == p {
			other.path = ""
		}
	}
	if er := s.native.Symlink(s.ctx, strings.ReplaceAll(target, "\\", "/"), p); er != 0 {
		return nil, er
	}
	native, er := s.native.Lopen(s.ctx, p, 0)
	if er != 0 {
		return nil, er
	}
	if er = f.file.Close(s.ctx); er != 0 {
		_ = native.Close(s.ctx)
		return nil, er
	}
	f.file = native
	f.path = p
	return s.attr(f)
}
func (s *FS) Link(source, parent vfs.VfsNode, name string) (*vfs.Attributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, syscall.EBADF
	}
	if e := s.writable(nil); e != nil {
		return nil, e
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00:") {
		return nil, syscall.EINVAL
	}
	if parent == vfs.VFS_ROOT_NODE && (name == meta.TrashName || jvfs.IsSpecialName(name)) {
		return nil, syscall.EACCES
	}
	var src, dir *handle
	for _, f := range s.handles {
		if vfs.VfsNode(f.file.Inode()) == source {
			src = f
		}
		if vfs.VfsNode(f.file.Inode()) == parent {
			dir = f
		}
	}
	if src == nil {
		return nil, syscall.EBADF
	}
	if e := s.writable(src); e != nil {
		return nil, e
	}
	if parent != vfs.VFS_ROOT_NODE {
		if dir == nil {
			return nil, syscall.EBADF
		}
		if e := s.writable(dir); e != nil {
			return nil, e
		}
		if e := s.pathIdentity(dir); e != nil {
			return nil, e
		}
	}
	var a meta.Attr
	if er := s.meta.Link(s.ctx, meta.Ino(source), meta.Ino(parent), name, &a); er != 0 {
		return nil, er
	}
	s.native.InvalidateEntry(meta.Ino(parent), name)
	return attributes(meta.Ino(source), &a), nil
}
