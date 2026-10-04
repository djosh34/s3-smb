package smbfs

import (
	"context"
	"errors"
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
	if err := admitted(smb.Inode(ino), &a); err != nil {
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
	if err := admitted(name.Parent, &a); err != nil {
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

// ReadDir uses stable SQLite edge IDs with no adapter cursor or attr snapshot.
func (s *FS) ReadDir(ctx context.Context, ino smb.Inode, cookie smb.Cookie, limit uint32) ([]smb.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit == 0 {
		return nil, smb.ErrInvalidParameter
	}
	var raw meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &raw)); err != nil {
		return nil, err
	}
	if err := admitted(ino, &raw); err != nil {
		return nil, err
	}
	if raw.Typ != meta.TypeDirectory {
		return nil, smb.ErrNotDirectory
	}
	if err := backendError(s.metadata.Access(storageContext(ctx), meta.Ino(ino), meta.MODE_MASK_R|meta.MODE_MASK_X, &raw)); err != nil {
		return nil, err
	}
	// The page query joins only live names and inodes. It needs no follow-up
	// lookup that could fail because a name was removed after enumeration.
	release := s.guardParent(ino)
	defer release()
	var out []smb.DirEntry
	for uint64(len(out)) < uint64(limit) {
		generation := s.directoryGeneration()
		entries, err := s.directoryPage(ctx, ino, cookie, min(limit, 512))
		if err != nil {
			return nil, storageError(err)
		}
		if len(entries) == 0 {
			break
		}
		for _, entry := range entries {
			cookie = entry.next
			if !validBase(entry.name) {
				continue
			}
			if err := admitted(entry.inode, &entry.attr); err != nil {
				continue
			}
			a, err := s.directoryAttr(ctx, entry, generation)
			if errors.Is(err, smb.ErrNameNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, smb.DirEntry{Name: entry.name, Attr: a, Next: cookie})
			if uint64(len(out)) == uint64(limit) {
				return out, nil
			}
		}
	}
	return out, nil
}

type directoryGeneration struct {
	commits uint64
	flushes uint64
}

func (s *FS) directoryGeneration() directoryGeneration {
	return directoryGeneration{commits: s.commits.Load(), flushes: s.flushes.Load()}
}

func (s *FS) directoryAttr(ctx context.Context, entry directoryEntry, generation directoryGeneration) (smb.Attr, error) {
	st, unpin := s.pin(entry.inode)
	live := st.snapshot()
	unpin()
	// Flush, truncate and reopen can invalidate a row while live length stays
	// valid. Reread only the affected inode, not entries beside unrelated I/O.
	// Last close drops the live length, so it still needs the commit counter.
	if live.flushed > generation.flushes || (!live.valid && s.commits.Load() != generation.commits) {
		return s.GetAttr(ctx, smb.ObjectKey{Inode: entry.inode})
	}
	return decorateAttr(entry.inode, &entry.attr, entry.values, live)
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
		st, done := s.acquire(expect)
		defer done()
		if err := backendError(s.metadata.RemoveXattr(storageContext(ctx), meta.Ino(expect), name.Stream)); err != nil {
			return err
		}
		st.forgetStream(name.Stream)
		return nil
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
	// All renames share this namespace-only guard so ancestry cannot change
	// between the directory check and mutation. File I/O never takes it.
	s.renameMu.Lock()
	defer s.renameMu.Unlock()
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
	source, err := s.expected(ctx, r.Source, r.SourceInode)
	if err != nil {
		return err
	}
	if source.Typ == meta.TypeDirectory {
		if err = s.checkAncestry(ctx, r.SourceInode, r.Destination.Parent); err != nil {
			return err
		}
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

func (s *FS) checkAncestry(ctx context.Context, source, parent smb.Inode) error {
	seen := make(map[smb.Inode]bool)
	for parent != smb.Inode(meta.RootInode) {
		if parent == source {
			return smb.ErrInvalidParameter
		}
		if parent == 0 || seen[parent] {
			return smb.ErrIO
		}
		seen[parent] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		var attr meta.Attr
		if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(parent), &attr)); err != nil {
			return err
		}
		if err := admitted(parent, &attr); err != nil {
			return err
		}
		parent = smb.Inode(attr.Parent)
	}
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
	if a.Parent.IsTrash() || a.Nlink == 0 {
		return "", smb.ErrNameNotFound
	}
	if err := admitted(ino, &a); err != nil {
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
		return "", smb.ErrNameNotFound
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
