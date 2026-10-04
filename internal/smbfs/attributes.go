package smbfs

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
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

func volumeIdentity(uuid string) uint64 {
	var hash uint64 = 14695981039346656037
	for _, b := range []byte(uuid) {
		hash ^= uint64(b)
		hash *= 1099511628211
	}
	return hash
}

func supported(ino smb.Inode, a *meta.Attr) error {
	if ino == 0 {
		return smb.ErrInvalidParameter
	}
	if vfs.IsSpecialNode(meta.Ino(ino)) || a.Parent.IsTrash() || !meta.Ino(ino).IsNormal() {
		return smb.ErrAccessDenied
	}
	if a.Typ != meta.TypeFile && a.Typ != meta.TypeDirectory {
		return smb.ErrNotSupported
	}
	return nil
}

func (s *FS) baseAttr(ctx context.Context, ino smb.Inode, st *inodeState) (smb.Attr, error) {
	if err := ctx.Err(); err != nil {
		return smb.Attr{}, err
	}
	if ino == 0 {
		return smb.Attr{}, smb.ErrInvalidParameter
	}
	var a meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(ino), &a)); err != nil {
		return smb.Attr{}, err
	}
	if err := supported(ino, &a); err != nil {
		return smb.Attr{}, err
	}
	out := smb.Attr{Inode: ino, Size: a.Length, Accessed: time.Unix(a.Atime, int64(a.Atimensec)).UTC(), Modified: time.Unix(a.Mtime, int64(a.Mtimensec)).UTC(), Changed: time.Unix(a.Ctime, int64(a.Ctimensec)).UTC(), Attributes: 0x80}
	if a.Typ == meta.TypeDirectory {
		out.Kind = smb.KindDirectory
		out.Size = 0
		out.Attributes = 0x10
	}
	out.Created = out.Changed
	var value []byte
	eno := s.metadata.GetXattr(storageContext(ctx), meta.Ino(ino), birthKey, &value)
	if eno != 0 && !errors.Is(eno, meta.ENOATTR) {
		return smb.Attr{}, backendError(eno)
	}
	if eno == 0 {
		created, parseErr := time.Parse(time.RFC3339Nano, string(value))
		if parseErr != nil {
			return smb.Attr{}, storageError(parseErr)
		}
		out.Created = created.UTC()
	}
	eno = s.metadata.GetXattr(storageContext(ctx), meta.Ino(ino), attributesKey, &value)
	if eno != 0 && !errors.Is(eno, meta.ENOATTR) {
		return smb.Attr{}, backendError(eno)
	}
	if eno == 0 {
		if len(value) != 4 {
			return smb.Attr{}, smb.ErrIO
		}
		out.Attributes = binary.LittleEndian.Uint32(value)
		if out.Kind == smb.KindDirectory {
			out.Attributes |= 0x10
		} else {
			out.Attributes &^= 0x10
		}
	}
	var err error
	out.Accessed, err = s.explicitTime(ctx, ino, accessedKey, out.Accessed)
	if err != nil {
		return smb.Attr{}, err
	}
	out.Modified, err = s.explicitTime(ctx, ino, modifiedKey, out.Modified)
	if err != nil {
		return smb.Attr{}, err
	}
	out.Changed, err = s.explicitTime(ctx, ino, changedKey, out.Changed)
	if err != nil {
		return smb.Attr{}, err
	}
	if st.dirty && out.Kind == smb.KindFile {
		out.Size = st.size
		out.Modified = st.modified
		out.Changed = st.modified
	}
	out.AllocationSize = allocation(out.Size)
	return out, nil
}

func (s *FS) explicitTime(ctx context.Context, ino smb.Inode, key string, actual time.Time) (time.Time, error) {
	var value []byte
	eno := s.metadata.GetXattr(storageContext(ctx), meta.Ino(ino), key, &value)
	if errors.Is(eno, meta.ENOATTR) {
		return actual, nil
	}
	if eno != 0 {
		return time.Time{}, backendError(eno)
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

func (s *FS) touchStream(ctx context.Context, ino smb.Inode) error {
	var attr meta.Attr
	return backendError(s.metadata.SetAttr(storageContext(ctx), meta.Ino(ino), meta.SetAttrMtimeNow, 0, &attr))
}

func (s *FS) stream(ctx context.Context, key smb.ObjectKey) ([]byte, error) {
	if !validStream(key.Stream) {
		return nil, smb.ErrInvalidName
	}
	var data []byte
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
	st, release := s.acquire(key.Inode)
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
	if _, err := s.attr(ctx, key, st); err != nil {
		return err
	}
	if change.Size != nil {
		if err := s.truncate(ctx, key, st, *change.Size); err != nil {
			return err
		}
	}
	if err := s.flush(ctx, st); err != nil {
		return err
	}
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
	if mask != 0 {
		if err := backendError(s.metadata.SetAttr(storageContext(ctx), meta.Ino(key.Inode), mask, 0, &a)); err != nil {
			return err
		}
	}
	// JuiceFS treats negative atime/mtime as "now" and always generates ctime.
	// Keep the exact requested value alongside its backend timestamp. A later
	// backend change invalidates this value, so it is not update suppression.
	var stored meta.Attr
	if err := backendError(s.metadata.GetAttr(storageContext(ctx), meta.Ino(key.Inode), &stored)); err != nil {
		return err
	}
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
		if err := s.storeTime(ctx, key.Inode, field.key, *field.wanted, field.actual); err != nil {
			return err
		}
	}
	if change.Created != nil {
		data, marshalErr := change.Created.UTC().MarshalText()
		if marshalErr != nil {
			return errors.Join(smb.ErrInvalidParameter, marshalErr)
		}
		if err := backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(key.Inode), birthKey, data, 0)); err != nil {
			return err
		}
	}
	if change.Attributes != nil {
		data := make([]byte, 4)
		binary.LittleEndian.PutUint32(data, *change.Attributes)
		if err := backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(key.Inode), attributesKey, data, 0)); err != nil {
			return err
		}
	}
	s.filesystem.InvalidateAttr(meta.Ino(key.Inode))
	return nil
}

// Streams lists only named data, not the adapter's private attributes.
func (s *FS) Streams(ctx context.Context, ino smb.Inode) ([]smb.StreamInfo, error) {
	st, release := s.acquire(ino)
	defer release()
	if _, err := s.baseAttr(ctx, ino, st); err != nil {
		return nil, err
	}
	var data []byte
	if err := backendError(s.metadata.ListXattr(storageContext(ctx), meta.Ino(ino), &data)); err != nil {
		return nil, err
	}
	var result []smb.StreamInfo
	for _, name := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
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
