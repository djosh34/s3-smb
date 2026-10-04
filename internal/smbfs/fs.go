package smbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"syscall"
	"time"

	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
)

// UID and GID identify the single storage account.
const (
	UID uint32 = 65534
	GID uint32 = 65534
)

// FS coordinates shared JuiceFS file references, not SMB opens. The map mutex
// only pins inode state. Each inode mutex owns its file reference and live size.
// No backend work runs while the map mutex is held.
type FS struct {
	filesystem *jfs.FileSystem
	metadata   meta.Meta
	barrier    MetadataBarrier
	inodes     map[smb.Inode]*inodeState
	parents    map[smb.Inode]*parentGuard
	mu         sync.Mutex
	capacity   uint64
	volumeID   uint64
	readOnly   bool
}

type inodeState struct {
	modified time.Time
	file     *jfs.File
	mu       sync.Mutex
	size     uint64
	users    int
	refs     int
	dirty    bool
}

type handle struct {
	owner  *FS
	state  *inodeState
	key    smb.ObjectKey
	access smb.Access
	closed bool
}

func (h *handle) Key() smb.ObjectKey { return h.key }

var _ smb.Storage = (*FS)(nil)

// New constructs an adapter. The caller owns the JuiceFS runtime.
func New(options Options) (*FS, error) {
	if options.Filesystem == nil || options.Barrier == nil {
		return nil, smb.ErrInvalidParameter
	}
	format, err := options.Filesystem.Meta().Load(false)
	if err != nil {
		return nil, storageError(err)
	}
	return &FS{
		filesystem: options.Filesystem, metadata: options.Filesystem.Meta(), barrier: options.Barrier,
		inodes: make(map[smb.Inode]*inodeState), parents: make(map[smb.Inode]*parentGuard), capacity: options.Capacity,
		volumeID: volumeIdentity(format.UUID), readOnly: options.ReadOnly,
	}, nil
}

func storageContext(ctx context.Context) meta.Context {
	return meta.WrapWithCancel(ctx, 1, UID, []uint32{GID})
}

func storageError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	var kind smb.ErrorKind
	if errors.As(err, &kind) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	kind = smb.ErrIO
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, meta.ENOATTR):
		kind = smb.ErrNameNotFound
	case errors.Is(err, syscall.EEXIST):
		kind = smb.ErrNameCollision
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		kind = smb.ErrAccessDenied
	case errors.Is(err, syscall.EROFS):
		kind = smb.ErrReadOnly
	case errors.Is(err, syscall.EBADF):
		kind = smb.ErrInvalidHandle
	case errors.Is(err, syscall.EINVAL):
		kind = smb.ErrInvalidParameter
	case errors.Is(err, syscall.ENOTDIR):
		kind = smb.ErrNotDirectory
	case errors.Is(err, syscall.EISDIR):
		kind = smb.ErrIsDirectory
	case errors.Is(err, syscall.ENOTEMPTY):
		kind = smb.ErrDirectoryNotEmpty
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		kind = smb.ErrDiskFull
	case errors.Is(err, syscall.EFBIG), errors.Is(err, syscall.E2BIG):
		kind = smb.ErrFileTooLarge
	case errors.Is(err, syscall.ENOTSUP):
		kind = smb.ErrNotSupported
	}
	return fmt.Errorf("%w: %w", kind, err)
}

func backendError(eno syscall.Errno) error {
	if eno == 0 {
		return nil
	}
	return storageError(eno)
}

func (s *FS) acquire(ino smb.Inode) (*inodeState, func()) {
	s.mu.Lock()
	st := s.inodes[ino]
	if st == nil {
		st = &inodeState{}
		s.inodes[ino] = st
	}
	st.users++
	s.mu.Unlock()
	st.mu.Lock()
	return st, func() {
		s.mu.Lock()
		st.users--
		if st.users == 0 && st.refs == 0 {
			delete(s.inodes, ino)
		}
		s.mu.Unlock()
		st.mu.Unlock()
	}
}

func (s *FS) selected(ctx context.Context, ref smb.Handle, write bool) (*handle, func(), error) {
	h, ok := ref.(*handle)
	if !ok || h == nil || h.owner != s {
		return nil, nil, smb.ErrInvalidHandle
	}
	st, release := s.acquire(h.key.Inode)
	if st != h.state || h.closed {
		release()
		return nil, nil, smb.ErrInvalidHandle
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, nil, err
	}
	if write && s.readOnly {
		release()
		return nil, nil, smb.ErrReadOnly
	}
	if write && h.access&smb.AccessWrite == 0 {
		release()
		return nil, nil, smb.ErrAccessDenied
	}
	return h, release, nil
}

// Open retains the shared native reference without changing data.
func (s *FS) Open(ctx context.Context, key smb.ObjectKey, access smb.Access) (smb.Handle, error) {
	if access & ^(smb.AccessRead|smb.AccessWrite) != 0 {
		return nil, smb.ErrInvalidParameter
	}
	if s.readOnly && access&smb.AccessWrite != 0 {
		return nil, smb.ErrReadOnly
	}
	st, release := s.acquire(key.Inode)
	defer release()
	if _, err := s.attr(ctx, key, st); err != nil {
		return nil, err
	}
	if st.file == nil {
		p, err := s.PathOf(ctx, key.Inode)
		if err != nil {
			return nil, err
		}
		flags := uint32(vfs.MODE_MASK_R | vfs.MODE_MASK_W)
		if s.readOnly {
			flags = vfs.MODE_MASK_R
		}
		file, eno := s.filesystem.Open(storageContext(ctx), "/"+p, flags)
		if eno != 0 {
			return nil, backendError(eno)
		}
		if smb.Inode(file.Inode()) != key.Inode {
			return nil, errors.Join(smb.ErrIdentityChanged, backendError(file.Close(storageContext(ctx)))) //nolint:contextcheck // JuiceFS Close uses background contexts internally despite receiving this context.
		}
		st.file = file
	}
	st.refs++
	return &handle{owner: s, state: st, key: key, access: access}, nil
}

func (s *FS) flush(ctx context.Context, st *inodeState) error {
	if st.file != nil {
		if err := backendError(st.file.Fsync(storageContext(ctx))); err != nil { //nolint:contextcheck // JuiceFS Fsync uses background contexts internally despite receiving this context.
			return err
		}
	}
	st.dirty = false
	return nil
}

// Close releases exactly one reference, propagating flush and close errors.
func (s *FS) Close(ctx context.Context, ref smb.Handle) error {
	h, release, err := s.selected(ctx, ref, false)
	if err != nil {
		return err
	}
	defer release()
	h.closed = true
	flushErr := s.flush(ctx, h.state)
	h.state.refs--
	if h.state.refs != 0 {
		return flushErr
	}
	closeErr := backendError(h.state.file.Close(storageContext(ctx))) //nolint:contextcheck // JuiceFS Close uses background contexts internally despite receiving this context.
	h.state.file = nil
	return errors.Join(flushErr, closeErr)
}

// Flush commits all writes on the inode before applying the metadata barrier.
func (s *FS) Flush(ctx context.Context, ref smb.Handle, mode smb.SyncMode) error {
	if mode != smb.SyncData && mode != smb.SyncFull {
		return smb.ErrInvalidParameter
	}
	h, release, err := s.selected(ctx, ref, false)
	if err != nil {
		return err
	}
	defer release()
	if err = s.flush(ctx, h.state); err != nil {
		return err
	}
	return storageError(s.barrier.Commit(ctx, mode == smb.SyncFull))
}

// ReadAt reads the selected data object, including another handle's writes.
func (s *FS) ReadAt(ctx context.Context, ref smb.Handle, dst []byte, offset uint64) (int, error) {
	h, release, err := s.selected(ctx, ref, false)
	if err != nil {
		return 0, err
	}
	defer release()
	if h.access&smb.AccessRead == 0 {
		return 0, smb.ErrAccessDenied
	}
	if offset > math.MaxInt64 || uint64(len(dst)) > math.MaxInt64-offset {
		return 0, smb.ErrInvalidParameter
	}
	if len(dst) == 0 {
		return 0, nil
	}
	a, err := s.attr(ctx, h.key, h.state)
	if err != nil {
		return 0, err
	}
	if a.Kind == smb.KindDirectory {
		return 0, smb.ErrIsDirectory
	}
	if h.key.Stream != "" {
		data, streamErr := s.stream(ctx, h.key)
		if streamErr != nil {
			return 0, streamErr
		}
		if offset >= uint64(len(data)) {
			return 0, io.EOF
		}
		n := copy(dst, data[offset:])
		if n < len(dst) {
			return n, io.EOF
		}
		return n, nil
	}
	n, err := h.state.file.Pread(storageContext(ctx), dst, int64(offset))
	if err == nil && n < len(dst) {
		err = io.EOF
	}
	return n, storageError(err)
}

// WriteAt writes with file offsets and zero-filled holes for both object kinds.
func (s *FS) WriteAt(ctx context.Context, ref smb.Handle, src []byte, offset uint64) (int, error) {
	h, release, err := s.selected(ctx, ref, true)
	if err != nil {
		return 0, err
	}
	defer release()
	if offset > math.MaxInt64 || uint64(len(src)) > math.MaxInt64-offset {
		return 0, smb.ErrInvalidParameter
	}
	a, err := s.attr(ctx, h.key, h.state)
	if err != nil {
		return 0, err
	}
	if a.Kind == smb.KindDirectory {
		return 0, smb.ErrIsDirectory
	}
	if len(src) == 0 {
		return 0, nil
	}
	if h.key.Stream != "" {
		end := offset + uint64(len(src))
		if end > maxStreamSize {
			return 0, smb.ErrFileTooLarge
		}
		data, err := s.stream(ctx, h.key)
		if err != nil {
			return 0, err
		}
		if end > uint64(len(data)) {
			data = append(data, make([]byte, int(end)-len(data))...)
		}
		copy(data[offset:], src)
		err = backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(h.key.Inode), h.key.Stream, data, meta.XattrReplace))
		if err != nil {
			return 0, err
		}
		if err = s.touchStream(ctx, h.key.Inode); err != nil {
			return 0, err
		}
		return len(src), nil
	}
	n, eno := h.state.file.Pwrite(storageContext(ctx), src, int64(offset)) //nolint:contextcheck // JuiceFS Pwrite uses background contexts internally despite receiving this context.
	if eno != 0 {
		return n, backendError(eno)
	}
	if n != len(src) {
		return n, storageError(io.ErrShortWrite)
	}
	h.state.size = max(a.Size, offset+uint64(len(src)))
	h.state.modified = time.Now().UTC()
	h.state.dirty = true
	return n, nil
}

func (s *FS) truncate(ctx context.Context, key smb.ObjectKey, st *inodeState, size uint64) error {
	if size > math.MaxInt64 {
		return smb.ErrInvalidParameter
	}
	a, err := s.attr(ctx, key, st)
	if err != nil {
		return err
	}
	if a.Kind == smb.KindDirectory {
		return smb.ErrIsDirectory
	}
	if key.Stream != "" {
		if size > maxStreamSize {
			return smb.ErrFileTooLarge
		}
		data, streamErr := s.stream(ctx, key)
		if streamErr != nil {
			return streamErr
		}
		if size > uint64(len(data)) {
			data = append(data, make([]byte, int(size)-len(data))...)
		} else {
			data = data[:size]
		}
		if err = backendError(s.metadata.SetXattr(storageContext(ctx), meta.Ino(key.Inode), key.Stream, data, meta.XattrReplace)); err != nil {
			return err
		}
		return s.touchStream(ctx, key.Inode)
	}
	// Drain the shared writer before changing length. Its old slices can no
	// longer commit after this truncate, even if another handle later flushes.
	if err = s.flush(ctx, st); err != nil {
		return err
	}
	if st.file != nil {
		return backendError(st.file.Truncate(storageContext(ctx), size)) //nolint:contextcheck // JuiceFS Truncate uses background contexts internally despite receiving this context.
	}
	return backendError(s.metadata.Truncate(storageContext(ctx), meta.Ino(key.Inode), 0, size, nil, false))
}

// Truncate changes only the selected object's length.
func (s *FS) Truncate(ctx context.Context, ref smb.Handle, size uint64) error {
	h, release, err := s.selected(ctx, ref, true)
	if err != nil {
		return err
	}
	defer release()
	return s.truncate(ctx, h.key, h.state, size)
}
