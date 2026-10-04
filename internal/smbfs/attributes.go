package smbfs

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"strings"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
)

const (
	maxStreamSize = 64 << 10
	privatePrefix = "s3-smb.internal."
	birthKey      = privatePrefix + "created"
	attributesKey = privatePrefix + "attributes"
	accessedKey   = privatePrefix + "accessed"
	modifiedKey   = privatePrefix + "modified"
	changedKey    = privatePrefix + "changed"
)

func volumeIdentity(uuid string) (uint64, error) {
	hash := fnv.New64a()
	if _, err := hash.Write([]byte(uuid)); err != nil {
		return 0, err
	}
	return hash.Sum64(), nil
}

func supported(ino smb.Inode, a *meta.Attr) error {
	if ino == 0 {
		return smb.ErrInvalidParameter
	}
	if vfs.IsSpecialNode(meta.Ino(ino)) || !meta.Ino(ino).IsNormal() {
		return smb.ErrAccessDenied
	}
	if a.Typ != meta.TypeFile && a.Typ != meta.TypeDirectory {
		return smb.ErrNotSupported
	}
	return nil
}

func admitted(ino smb.Inode, a *meta.Attr) error {
	if a.Parent.IsTrash() {
		return smb.ErrAccessDenied
	}
	return supported(ino, a)
}

func (s *FS) baseAttr(ctx context.Context, ino smb.Inode, st *inodeState) (smb.Attr, error) {
	if err := ctx.Err(); err != nil {
		return smb.Attr{}, err
	}
	if ino == 0 {
		return smb.Attr{}, smb.ErrInvalidParameter
	}
	live := st.snapshot()
	var a meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &a)); err != nil {
		return smb.Attr{}, err
	}
	if err := supported(ino, &a); err != nil {
		return smb.Attr{}, err
	}
	if a.Parent.IsTrash() && st.refs.Load() == 0 {
		return smb.Attr{}, smb.ErrAccessDenied
	}
	var values privateAttrs
	for index, key := range privateKeys {
		eno := s.metadata.GetXattr(storageContext(ctx), meta.Ino(ino), key, &values[index])
		if eno != 0 && !errors.Is(eno, meta.ENOATTR) {
			return smb.Attr{}, backendError(eno)
		}
	}
	return decorateAttr(ino, &a, values, live)
}

type privateAttrs [5][]byte

var privateKeys = [5]string{birthKey, attributesKey, accessedKey, modifiedKey, changedKey}

func decorateAttr(ino smb.Inode, a *meta.Attr, values privateAttrs, live liveState) (smb.Attr, error) {
	out := smb.Attr{Inode: ino, Size: a.Length, Accessed: time.Unix(a.Atime, int64(a.Atimensec)).UTC(), Modified: time.Unix(a.Mtime, int64(a.Mtimensec)).UTC(), Changed: time.Unix(a.Ctime, int64(a.Ctimensec)).UTC(), Attributes: 0x80}
	if a.Typ == meta.TypeDirectory {
		out.Kind = smb.KindDirectory
		out.Size = 0
		out.Attributes = 0x10
	}
	out.Created = out.Changed
	if len(values[0]) != 0 {
		created, err := time.Parse(time.RFC3339Nano, string(values[0]))
		if err != nil {
			return smb.Attr{}, storageError(err)
		}
		out.Created = created.UTC()
	}
	if values[1] != nil {
		if len(values[1]) != 4 {
			return smb.Attr{}, smb.ErrIO
		}
		out.Attributes = binary.LittleEndian.Uint32(values[1])
		if out.Kind == smb.KindDirectory {
			out.Attributes |= 0x10
		} else {
			out.Attributes &^= 0x10
		}
	}
	var err error
	out.Accessed, err = explicitTime(values[2], out.Accessed)
	if err != nil {
		return smb.Attr{}, err
	}
	out.Modified, err = explicitTime(values[3], out.Modified)
	if err != nil {
		return smb.Attr{}, err
	}
	out.Changed, err = explicitTime(values[4], out.Changed)
	if err != nil {
		return smb.Attr{}, err
	}
	if live.valid && out.Kind == smb.KindFile {
		out.Size = live.size
	}
	if live.dirty && out.Kind == smb.KindFile {
		out.Size = live.size
		out.Modified = live.modified
		out.Changed = live.modified
	}
	out.AllocationSize = allocation(out.Size)
	return out, nil
}

func explicitTime(value []byte, actual time.Time) (time.Time, error) {
	if value == nil {
		return actual, nil
	}
	fields := strings.Split(string(value), "\n")
	if len(fields) != 2 {
		return time.Time{}, smb.ErrIO
	}
	wanted, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return time.Time{}, storageError(err)
	}
	reference, err := time.Parse(time.RFC3339Nano, fields[1])
	if err != nil {
		return time.Time{}, storageError(err)
	}
	if !reference.Equal(actual) {
		return actual, nil
	}
	return wanted.UTC(), nil
}

// JuiceFS drops xattrs at unlink, not when a sustained inode is finally closed.
// Retained-reference writes may restore them, so remove them before that close.
func (s *FS) clearUnlinkedXattrs(ctx context.Context, ino smb.Inode) error {
	var attr meta.Attr
	eno := s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &attr)
	if eno == syscall.ENOENT {
		return nil
	}
	if eno != 0 {
		return backendError(eno)
	}
	if attr.Nlink != 0 || attr.Parent.IsTrash() {
		return nil
	}
	var names []byte
	if err := backendError(s.metadata.ListXattr(storageContext(ctx), meta.Ino(ino), &names)); err != nil {
		return err
	}
	var result error
	for _, name := range strings.Split(strings.TrimSuffix(string(names), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		eno = s.metadata.RemoveXattr(storageContext(ctx), meta.Ino(ino), name)
		if eno != 0 && !errors.Is(eno, meta.ENOATTR) {
			result = errors.Join(result, backendError(eno))
		}
	}
	return result
}

func (s *FS) storeTime(ctx context.Context, ino smb.Inode, key string, wanted, actual time.Time) error {
	requested, err := wanted.UTC().MarshalText()
	if err != nil {
		return errors.Join(smb.ErrInvalidParameter, err)
	}
	requested = append(requested, '\n')
	requested = append(requested, actual.UTC().Format(time.RFC3339Nano)...)
	return backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(ino), key, requested, 0))
}

func allocation(size uint64) uint64 { return (size + 4095) / 4096 * 4096 }

func nanoseconds(stamp time.Time) uint32 {
	ns := stamp.Nanosecond()
	if ns < 0 || ns >= 1e9 {
		return 0
	}
	return uint32(ns)
}

func validStream(name string) bool {
	return name != "" && len(name) <= 255 && !strings.ContainsAny(name, "\x00:/\\") && !strings.HasPrefix(name, privatePrefix)
}

func (s *FS) touchStream(ctx context.Context, ino smb.Inode, st *inodeState) error {
	var attr meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &attr)); err != nil {
		return err
	}
	defer s.invalidateDirectoryRow(st)
	if !attr.Parent.IsTrash() {
		return backendError(s.metadata.SetAttr(storageContext(ctx), meta.Ino(ino), meta.SetAttrMtimeNow, 0, &attr))
	}
	now := time.Now().UTC()
	if err := s.storeTime(ctx, ino, modifiedKey, now, time.Unix(attr.Mtime, int64(attr.Mtimensec))); err != nil {
		return err
	}
	return s.storeTime(ctx, ino, changedKey, now, time.Unix(attr.Ctime, int64(attr.Ctimensec)))
}

// Stream snapshots are immutable and belong to inode coherence, not SMB open
// policy. JuiceFS may remove xattrs at unlink even while the inode is sustained.
func (st *inodeState) cachedStream(name string) ([]byte, bool) {
	st.liveMu.RLock()
	defer st.liveMu.RUnlock()
	data, ok := st.streams[name]
	return data, ok
}

func (st *inodeState) publishStream(name string, data []byte) {
	st.liveMu.Lock()
	defer st.liveMu.Unlock()
	if st.streams == nil {
		st.streams = make(map[string][]byte)
	}
	st.streams[name] = data
}

func (st *inodeState) forgetStream(name string) {
	st.liveMu.Lock()
	defer st.liveMu.Unlock()
	delete(st.streams, name)
}

func (s *FS) saveStream(ctx context.Context, key smb.ObjectKey, data []byte) error {
	eno := s.metadata.SetXattr(storageContext(ctx), meta.Ino(key.Inode), key.Stream, data, meta.XattrReplace)
	if errors.Is(eno, meta.ENOATTR) {
		var attr meta.Attr
		if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(key.Inode), &attr)); err != nil {
			return err
		}
		// A retained stream still selects its old inode after unlink. Restore its
		// metadata value, rather than treating unlink as a selected-stream delete.
		if attr.Nlink == 0 {
			eno = s.metadata.SetXattr(storageContext(ctx), meta.Ino(key.Inode), key.Stream, data, 0)
		}
	}
	if eno != 0 {
		return backendError(eno)
	}
	st, release := s.pin(key.Inode)
	defer release()
	if st.refs.Load() > 0 {
		st.publishStream(key.Stream, data)
	}
	return nil
}

func (s *FS) stream(ctx context.Context, key smb.ObjectKey) ([]byte, error) {
	if !validStream(key.Stream) {
		return nil, smb.ErrInvalidName
	}
	st, release := s.pin(key.Inode)
	data, cached := st.cachedStream(key.Stream)
	retained := st.refs.Load() > 0
	release()
	if cached && retained {
		return data, nil
	}
	data = nil
	err := backendError(s.metadata.GetXattr(storageContext(ctx), meta.Ino(key.Inode), key.Stream, &data))
	return data, err
}

func (s *FS) attr(ctx context.Context, key smb.ObjectKey, st *inodeState) (smb.Attr, error) {
	a, err := s.baseAttr(ctx, key.Inode, st)
	if err != nil {
		return smb.Attr{}, err
	}
	if key.Stream == "" {
		return a, nil
	}
	data, err := s.stream(ctx, key)
	if err != nil {
		return smb.Attr{}, err
	}
	a.Kind = smb.KindFile
	a.Attributes &^= 0x10
	a.Size = uint64(len(data))
	a.AllocationSize = allocation(a.Size)
	return a, nil
}

// GetAttr returns current attributes without forcing uploads.
func (s *FS) GetAttr(ctx context.Context, key smb.ObjectKey) (smb.Attr, error) {
	st, release := s.pin(key.Inode)
	defer release()
	return s.attr(ctx, key, st)
}

// SetAttr orders explicit changes after buffered data commits.
func (s *FS) SetAttr(ctx context.Context, key smb.ObjectKey, change smb.AttrChange) error {
	if s.readOnly {
		return smb.ErrReadOnly
	}
	st, release := s.acquire(key.Inode)
	defer release()
	if change.Size != nil && change.SizeCap != nil {
		return smb.ErrInvalidParameter
	}
	if change.Size != nil && *change.Size >= maxFileSize || change.SizeCap != nil && *change.SizeCap >= maxFileSize {
		return smb.ErrFileTooLarge
	}
	attr, err := s.attr(ctx, key, st)
	if err != nil {
		return err
	}
	if change.SizeCap != nil {
		if attr.Kind == smb.KindDirectory {
			return smb.ErrIsDirectory
		}
		if *change.SizeCap < attr.Size {
			change.Size = change.SizeCap
		}
	}
	if change.Accessed != nil || change.Modified != nil || change.Changed != nil || change.Created != nil || change.Attributes != nil {
		// A later error can leave some attributes changed, so invalidate on error too.
		defer s.invalidateDirectoryRow(st)
	}
	if change.Size != nil {
		if err := s.truncate(ctx, key, st, *change.Size); err != nil {
			return err
		}
	}
	if change.Accessed != nil || change.Modified != nil || change.Changed != nil {
		if err := s.flush(ctx, st); err != nil {
			return err
		}
	}
	if err := s.setTimes(ctx, key.Inode, change); err != nil {
		return err
	}
	if err := s.setProperties(ctx, key.Inode, change); err != nil {
		return err
	}
	s.filesystem.InvalidateAttr(meta.Ino(key.Inode))
	return nil
}

func (s *FS) setTimes(ctx context.Context, ino smb.Inode, change smb.AttrChange) error {
	var a meta.Attr
	var mask uint16
	if change.Accessed != nil {
		a.Atime = max(int64(0), change.Accessed.Unix())
		a.Atimensec = nanoseconds(*change.Accessed)
		mask |= meta.SetAttrAtime
	}
	if change.Modified != nil {
		a.Mtime = max(int64(0), change.Modified.Unix())
		a.Mtimensec = nanoseconds(*change.Modified)
		mask |= meta.SetAttrMtime
	}
	if change.Changed != nil {
		a.Ctime = change.Changed.Unix()
		a.Ctimensec = nanoseconds(*change.Changed)
		mask |= meta.SetAttrCtime
	}
	if mask == 0 {
		return nil
	}
	var stored meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &stored)); err != nil {
		return err
	}
	// JuiceFS forbids path-based time changes in trash. Retained references use
	// the same private exact-time representation without changing trash admission.
	if !stored.Parent.IsTrash() {
		if err := backendError(s.metadata.SetAttr(storageContext(ctx), meta.Ino(ino), mask, 0, &a)); err != nil {
			return err
		}
		if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &stored)); err != nil {
			return err
		}
	}
	// JuiceFS treats negative atime/mtime as "now" and always generates ctime.
	// Keep the exact requested value alongside its backend timestamp. A later
	// backend change invalidates this value, so it is not update suppression.
	for _, field := range []struct {
		actual time.Time
		wanted *time.Time
		key    string
	}{
		{time.Unix(stored.Atime, int64(stored.Atimensec)), change.Accessed, accessedKey},
		{time.Unix(stored.Mtime, int64(stored.Mtimensec)), change.Modified, modifiedKey},
		{time.Unix(stored.Ctime, int64(stored.Ctimensec)), change.Changed, changedKey},
	} {
		if field.wanted == nil {
			continue
		}
		if err := s.storeTime(ctx, ino, field.key, *field.wanted, field.actual); err != nil {
			return err
		}
	}
	return nil
}

func (s *FS) invalidateDirectoryRow(st *inodeState) {
	st.liveMu.Lock()
	st.live.flushed = s.flushes.Add(1)
	st.liveMu.Unlock()
	// Without a retained handle, the per-inode generation leaves with its state.
	if st.refs.Load() == 0 {
		s.commits.Add(1)
	}
}

func (s *FS) setProperties(ctx context.Context, ino smb.Inode, change smb.AttrChange) error {
	if change.Created != nil {
		data, marshalErr := change.Created.UTC().MarshalText()
		if marshalErr != nil {
			return errors.Join(smb.ErrInvalidParameter, marshalErr)
		}
		if err := backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(ino), birthKey, data, 0)); err != nil {
			return err
		}
	}
	if change.Attributes != nil {
		data := make([]byte, 4)
		binary.LittleEndian.PutUint32(data, *change.Attributes)
		if err := backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(ino), attributesKey, data, 0)); err != nil {
			return err
		}
	}
	return nil
}

// Streams lists only named data, not the adapter's private attributes.
func (s *FS) Streams(ctx context.Context, ino smb.Inode) ([]smb.StreamInfo, error) {
	st, release := s.pin(ino)
	defer release()
	if _, err := s.baseAttr(ctx, ino, st); err != nil {
		return nil, err
	}
	var data []byte
	if err := backendError(s.metadata.ListXattr(storageContext(ctx), meta.Ino(ino), &data)); err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for _, name := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
		names[name] = true
	}
	if st.refs.Load() > 0 {
		st.liveMu.RLock()
		for name := range st.streams {
			names[name] = true
		}
		st.liveMu.RUnlock()
	}
	var result []smb.StreamInfo
	for name := range names {
		if !validStream(name) {
			continue
		}
		value, err := s.stream(ctx, smb.ObjectKey{Inode: ino, Stream: name})
		if err != nil {
			return nil, err
		}
		size := uint64(len(value))
		result = append(result, smb.StreamInfo{Name: name, Size: size, AllocationSize: allocation(size)})
	}
	return result, nil
}

// StatFS keeps configured capacity intact, or caps unlimited free space at 1 TiB.
func (s *FS) StatFS(ctx context.Context) (smb.Space, error) {
	if err := ctx.Err(); err != nil {
		return smb.Space{}, err
	}
	var total, free, usedInodes, freeInodes uint64
	if err := backendError(s.metadata.StatFS(storageContext(ctx), meta.RootInode, &total, &free, &usedInodes, &freeInodes)); err != nil {
		return smb.Space{}, err
	}
	free = min(free, total)
	used := total - free
	if s.capacity != 0 {
		total = s.capacity
		free = total - min(total, used)
	} else if free > 1<<40 {
		free = 1 << 40
		total = used + free
	}
	return smb.Space{VolumeID: s.volumeID, Capacity: total, Free: free, Available: free}, nil
}
