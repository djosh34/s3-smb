package smbfs

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
)

type parentGuard struct {
	mu    sync.Mutex
	users int
}

// Parent guards are separate from data coherence to avoid lock-order cycles
// when a renamed directory is both a parent and a stream's base inode.
func (s *FS) guardParent(ino smb.Inode) func() {
	s.mu.Lock()
	guard := s.parents[ino]
	if guard == nil {
		guard = &parentGuard{}
		s.parents[ino] = guard
	}
	guard.users++
	s.mu.Unlock()
	guard.mu.Lock()
	return func() {
		s.mu.Lock()
		guard.users--
		if guard.users == 0 {
			delete(s.parents, ino)
		}
		s.mu.Unlock()
		guard.mu.Unlock()
	}
}

func validBase(base string) bool {
	return base != "" && base != "." && base != ".." && len(base) <= 255 && utf8.ValidString(base) && !strings.ContainsAny(base, "\x00:/\\") && base != meta.TrashName && !vfs.IsSpecialName(base)
}

func parsePath(p string) ([]string, string, error) {
	if !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return nil, "", smb.ErrInvalidName
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return nil, "", nil
	}
	if strings.HasPrefix(p, "/") {
		return nil, "", smb.ErrInvalidName
	}
	parts := strings.Split(p, "/")
	last := strings.Split(parts[len(parts)-1], ":")
	stream := ""
	switch len(last) {
	case 1:
	case 2:
		stream = last[1]
		if !validStream(stream) {
			return nil, "", smb.ErrInvalidName
		}
	case 3:
		if !strings.EqualFold(last[2], "$DATA") {
			return nil, "", smb.ErrNotSupported
		}
		stream = last[1]
		if stream != "" && !validStream(stream) {
			return nil, "", smb.ErrInvalidName
		}
	default:
		return nil, "", smb.ErrInvalidName
	}
	parts[len(parts)-1] = last[0]
	for _, part := range parts {
		if !validBase(part) {
			return nil, "", smb.ErrInvalidName
		}
	}
	return parts, stream, nil
}

func (s *FS) lookupEntry(ctx context.Context, parent smb.Inode, base string) (smb.Inode, meta.Attr, error) {
	if err := ctx.Err(); err != nil {
		return 0, meta.Attr{}, err
	}
	var ino meta.Ino
	var a meta.Attr
	eno := s.metadata.Lookup(storageContext(ctx), meta.Ino(parent), base, &ino, &a, true)
	if eno != 0 {
		return 0, a, backendError(eno)
	}
	if err := supported(smb.Inode(ino), &a); err != nil {
		return 0, a, err
	}
	return smb.Inode(ino), a, nil
}

// Lookup resolves names once, leaving missing final objects usable by Create.
func (s *FS) Lookup(ctx context.Context, p string) (smb.Resolved, error) {
	parts, stream, err := parsePath(p)
	if err != nil {
		return smb.Resolved{}, err
	}
	parent := smb.Inode(meta.RootInode)
	if len(parts) == 0 {
		key := smb.ObjectKey{Inode: parent}
		a, attrErr := s.GetAttr(ctx, key)
		return smb.Resolved{Object: key, Attr: a, Exists: attrErr == nil}, attrErr
	}
	for _, base := range parts[:len(parts)-1] {
		ino, a, lookupErr := s.lookupEntry(ctx, parent, base)
		if errors.Is(lookupErr, smb.ErrNameNotFound) {
			return smb.Resolved{}, errors.Join(smb.ErrPathNotFound, syscall.ENOENT)
		}
		if lookupErr != nil {
			return smb.Resolved{}, lookupErr
		}
		if a.Typ != meta.TypeDirectory {
			return smb.Resolved{}, smb.ErrNotDirectory
		}
		parent = ino
	}
	name := smb.Name{Parent: parent, Base: parts[len(parts)-1], Stream: stream}
	ino, _, err := s.lookupEntry(ctx, parent, name.Base)
	result := smb.Resolved{Name: name, Object: smb.ObjectKey{Inode: ino, Stream: stream}}
	if errors.Is(err, smb.ErrNameNotFound) {
		return result, nil
	}
	if err != nil {
		return smb.Resolved{}, err
	}
	result.Attr, err = s.GetAttr(ctx, result.Object)
	if errors.Is(err, smb.ErrNameNotFound) && stream != "" {
		return result, nil
	}
	result.Exists = err == nil
	return result, err
}

func (s *FS) checkName(ctx context.Context, name smb.Name) error {
	if !validBase(name.Base) || (name.Stream != "" && !validStream(name.Stream)) {
		return smb.ErrInvalidName
	}
	if name.Parent == 0 {
		return smb.ErrInvalidParameter
	}
	var a meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(name.Parent), &a)); err != nil {
		return err
	}
	if err := supported(name.Parent, &a); err != nil {
		return err
	}
	if a.Typ != meta.TypeDirectory {
		return smb.ErrNotDirectory
	}
	return ctx.Err()
}

// Create exclusively creates a base entry or one xattr on an existing base.
func (s *FS) Create(ctx context.Context, name smb.Name, kind smb.Kind) (smb.Resolved, error) {
	if s.readOnly {
		return smb.Resolved{}, smb.ErrReadOnly
	}
	if err := s.checkName(ctx, name); err != nil {
		return smb.Resolved{}, err
	}
	release := s.guardParent(name.Parent)
	defer release()
	var ino meta.Ino
	var a meta.Attr
	if name.Stream != "" {
		if kind != smb.KindFile {
			return smb.Resolved{}, smb.ErrNotDirectory
		}
		base, _, err := s.lookupEntry(ctx, name.Parent, name.Base)
		if err != nil {
			return smb.Resolved{}, err
		}
		st, done := s.acquire(base)
		defer done()
		if createErr := backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(base), name.Stream, []byte{}, meta.XattrCreate)); createErr != nil {
			return smb.Resolved{}, createErr
		}
		key := smb.ObjectKey{Inode: base, Stream: name.Stream}
		// The inode mutex is already held.
		attr, err := s.attr(ctx, key, st)
		return smb.Resolved{Name: name, Object: key, Attr: attr, Exists: err == nil}, err
	}
	var eno syscall.Errno
	switch kind {
	case smb.KindFile:
		eno = s.metadata.Mknod(storageContext(ctx), meta.Ino(name.Parent), name.Base, meta.TypeFile, 0o600, 0, 0, "", &ino, &a)
	case smb.KindDirectory:
		eno = s.metadata.Mkdir(storageContext(ctx), meta.Ino(name.Parent), name.Base, 0o700, 0, 0, &ino, &a)
	default:
		return smb.Resolved{}, smb.ErrInvalidParameter
	}
	if eno != 0 {
		return smb.Resolved{}, backendError(eno)
	}
	s.filesystem.InvalidateEntry(meta.Ino(name.Parent), name.Base)
	created, marshalErr := time.Unix(a.Ctime, int64(a.Ctimensec)).UTC().MarshalText()
	if marshalErr != nil {
		return smb.Resolved{}, storageError(marshalErr)
	}
	if err := backendError(s.metadata.SetXattr(storageContext(ctx), ino, birthKey, created, 0)); err != nil {
		var cleanup syscall.Errno
		if kind == smb.KindDirectory {
			cleanup = s.metadata.Rmdir(storageContext(ctx), meta.Ino(name.Parent), name.Base)
		} else {
			cleanup = s.metadata.Unlink(storageContext(ctx), meta.Ino(name.Parent), name.Base)
		}
		return smb.Resolved{}, errors.Join(err, backendError(cleanup))
	}
	key := smb.ObjectKey{Inode: smb.Inode(ino)}
	attr, err := s.GetAttr(ctx, key)
	return smb.Resolved{Name: name, Object: key, Attr: attr, Exists: err == nil}, err
}

// ReadDir uses a sorted position cookie, with no adapter cursor or attr snapshot.
func (s *FS) ReadDir(ctx context.Context, ino smb.Inode, cookie smb.Cookie, limit uint32) ([]smb.DirEntry, error) {
	a, err := s.GetAttr(ctx, smb.ObjectKey{Inode: ino})
	if err != nil {
		return nil, err
	}
	if a.Kind != smb.KindDirectory {
		return nil, smb.ErrNotDirectory
	}
	if limit == 0 {
		return nil, smb.ErrInvalidParameter
	}
	var entries []*meta.Entry
	if err := backendError(s.metadata.Readdir(storageContext(ctx), meta.Ino(ino), 1, &entries)); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := string(entry.Name)
		if validBase(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if uint64(cookie) >= uint64(len(names)) {
		return nil, nil
	}
	end := min(uint64(len(names)), uint64(cookie)+uint64(limit))
	var out []smb.DirEntry
	for i := uint64(cookie); i < end; i++ {
		child, _, err := s.lookupEntry(ctx, ino, names[i])
		if err != nil {
			return nil, err
		}
		a, err := s.GetAttr(ctx, smb.ObjectKey{Inode: child})
		if err != nil {
			return nil, err
		}
		out = append(out, smb.DirEntry{Name: names[i], Attr: a, Next: smb.Cookie(i + 1)})
	}
	return out, nil
}

func (s *FS) expected(ctx context.Context, name smb.Name, expect smb.Inode) (meta.Attr, error) {
	ino, a, err := s.lookupEntry(ctx, name.Parent, name.Base)
	if errors.Is(err, smb.ErrNameNotFound) {
		if expect == 0 {
			return a, nil
		}
		return a, smb.ErrIdentityChanged
	}
	if err != nil {
		return a, err
	}
	if ino != expect {
		return a, smb.ErrIdentityChanged
	}
	return a, nil
}

// Remove verifies the base identity before deleting the selected object.
func (s *FS) Remove(ctx context.Context, name smb.Name, expect smb.Inode) error {
	if s.readOnly {
		return smb.ErrReadOnly
	}
	if expect == 0 {
		return smb.ErrInvalidParameter
	}
	if err := s.checkName(ctx, name); err != nil {
		return err
	}
	release := s.guardParent(name.Parent)
	defer release()
	a, err := s.expected(ctx, name, expect)
	if err != nil {
		return err
	}
	if name.Stream != "" {
		_, done := s.acquire(expect)
		defer done()
		return backendError(s.metadata.RemoveXattr(storageContext(ctx), meta.Ino(expect), name.Stream))
	}
	var eno syscall.Errno
	if a.Typ == meta.TypeDirectory {
		eno = s.metadata.Rmdir(storageContext(ctx), meta.Ino(name.Parent), name.Base)
	} else {
		eno = s.metadata.Unlink(storageContext(ctx), meta.Ino(name.Parent), name.Base)
	}
	if eno != 0 {
		return backendError(eno)
	}
	s.filesystem.InvalidateEntry(meta.Ino(name.Parent), name.Base)
	return nil
}

// Rename checks both identities under ordered parent guards. Handles keep inodes.
func (s *FS) Rename(ctx context.Context, r smb.RenameRequest) error {
	if r.Source.Stream != "" || r.Destination.Stream != "" {
		return smb.ErrNotSupported
	}
	if s.readOnly {
		return smb.ErrReadOnly
	}
	if r.SourceInode == 0 {
		return smb.ErrInvalidParameter
	}
	if err := s.checkName(ctx, r.Source); err != nil {
		return err
	}
	if err := s.checkName(ctx, r.Destination); err != nil {
		return err
	}
	first, second := r.Source.Parent, r.Destination.Parent
	if first > second {
		first, second = second, first
	}
	release := s.guardParent(first)
	defer release()
	if first != second {
		done := s.guardParent(second)
		defer done()
	}
	if _, err := s.expected(ctx, r.Source, r.SourceInode); err != nil {
		return err
	}
	if _, err := s.expected(ctx, r.Destination, r.DestinationInode); err != nil {
		return err
	}
	flags := uint32(meta.RenameNoReplace)
	if r.Replace {
		flags = 0
	} else if r.DestinationInode != 0 {
		return smb.ErrNameCollision
	}
	var ino meta.Ino
	var a meta.Attr
	if err := backendError(s.metadata.Rename(storageContext(ctx), meta.Ino(r.Source.Parent), r.Source.Base, meta.Ino(r.Destination.Parent), r.Destination.Base, flags, &ino, &a)); err != nil {
		return err
	}
	s.filesystem.InvalidateEntry(meta.Ino(r.Source.Parent), r.Source.Base)
	s.filesystem.InvalidateEntry(meta.Ino(r.Destination.Parent), r.Destination.Base)
	return nil
}

// PathOf discovers the linked inode's current name, never a cached handle path.
func (s *FS) PathOf(ctx context.Context, ino smb.Inode) (string, error) {
	if ino == 0 {
		return "", smb.ErrInvalidParameter
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var a meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &a)); err != nil {
		return "", err
	}
	if err := supported(ino, &a); err != nil {
		return "", err
	}
	if ino == smb.Inode(meta.RootInode) {
		return "", nil
	}
	paths := s.metadata.GetPaths(storageContext(ctx), meta.Ino(ino))
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(paths) != 1 {
		return "", smb.ErrNameNotFound
	}
	p := strings.TrimPrefix(paths[0], "/")
	parts, stream, err := parsePath(p)
	if err != nil || stream != "" {
		return "", smb.ErrAccessDenied
	}
	parent := smb.Inode(meta.RootInode)
	for _, base := range parts {
		parent, _, err = s.lookupEntry(ctx, parent, base)
		if err != nil {
			return "", err
		}
	}
	if parent != ino {
		return "", smb.ErrIdentityChanged
	}
	return p, nil
}
