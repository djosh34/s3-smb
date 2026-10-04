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
		if len(value) != 12 {
			return smb.Attr{}, smb.ErrIO
		}
		out.Created = time.Unix(int64(binary.LittleEndian.Uint64(value)), int64(binary.LittleEndian.Uint32(value[8:]))).UTC()
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
	if st.dirty && out.Kind == smb.KindFile {
		out.Size = st.size
		out.Modified = st.modified
	}
	out.AllocationSize = allocation(out.Size)
	return out, nil
}

func allocation(size uint64) uint64 { return (size + 4095) / 4096 * 4096 }

func validStream(name string) bool {
	return name != "" && len(name) <= 255 && !strings.ContainsAny(name, "\x00:/\\") && !strings.HasPrefix(name, privatePrefix)
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
		a.Atime = change.Accessed.Unix()
		a.Atimensec = uint32(change.Accessed.Nanosecond())
		mask |= meta.SetAttrAtime
	}
	if change.Modified != nil {
		a.Mtime = change.Modified.Unix()
		a.Mtimensec = uint32(change.Modified.Nanosecond())
		mask |= meta.SetAttrMtime
	}
	if change.Changed != nil {
		a.Ctime = change.Changed.Unix()
		a.Ctimensec = uint32(change.Changed.Nanosecond())
		mask |= meta.SetAttrCtime
	}
	if mask != 0 {
		if err := backendError(s.metadata.SetAttr(storageContext(ctx), meta.Ino(key.Inode), mask, 0, &a)); err != nil {
			return err
		}
	}
	if change.Created != nil {
		data := make([]byte, 12)
		binary.LittleEndian.PutUint64(data, uint64(change.Created.Unix()))
		binary.LittleEndian.PutUint32(data[8:], uint32(change.Created.Nanosecond()))
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
