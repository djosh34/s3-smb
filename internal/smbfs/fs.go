package smbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
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
	// JuiceFS's VFS uses signed 31-bit chunk indexes and 64 MiB chunks.
	maxFileSize = meta.ChunkSize << 31
)

// FS owns per-inode I/O coordination. Attribute snapshots use a separate small
// lock, so metadata queries never wait for an inode's upload or cold read.
// The map mutex only pins state; the rename mutex protects directory ancestry.
type FS struct {
	filesystem   *jfs.FileSystem
	metadata     meta.Meta
	barrier      MetadataBarrier
	reader       vfs.DataReader
	writer       vfs.DataWriter
	inodes       map[smb.Inode]*inodeState
	parents      map[smb.Inode]*parentGuard
	metadataPath string
	mu           sync.Mutex
	renameMu     sync.Mutex
	capacity     uint64
	volumeID     uint64
	readOnly     bool
}

type liveState struct {
	modified time.Time
	size     uint64
	dirty    bool
}

type inodeState struct {
	reader  vfs.FileReader
	writer  vfs.FileWriter
	streams map[string][]byte
	live    liveState
	liveMu  sync.RWMutex
	mu      sync.Mutex
	refs    atomic.Int64
	users   int
}

type handle struct {
	owner  *FS
	state  *inodeState
	key    smb.ObjectKey
	access smb.Access
	kind   smb.Kind
	closed bool
}

func (h *handle) Key() smb.ObjectKey { return h.key }

var _ smb.Storage = (*FS)(nil)

// New constructs an adapter. The caller owns the JuiceFS runtime and chunk store.
func New(options Options) (*FS, error) {
	if options.Filesystem == nil || options.Barrier == nil || options.Config == nil || options.Config.Meta == nil || options.Config.Chunk == nil || options.Store == nil {
		return nil, smb.ErrInvalidParameter
	}
	if err := prepareDirectoryPages(options.MetadataPath, options.ReadOnly); err != nil {
		return nil, storageError(err)
	}
	m := options.Filesystem.Meta()
	volumeID, err := volumeIdentity(m.GetFormat().UUID)
	if err != nil {
		return nil, storageError(err)
	}
	reader := vfs.NewDataReader(options.Config, m, options.Store)
	writer := vfs.NewDataWriter(options.Config, m, options.Store, reader)
	return &FS{
		filesystem: options.Filesystem, metadata: m, barrier: options.Barrier, reader: reader, writer: writer,
		inodes: make(map[smb.Inode]*inodeState), parents: make(map[smb.Inode]*parentGuard), metadataPath: options.MetadataPath,
		capacity: options.Capacity, volumeID: volumeID, readOnly: options.ReadOnly,
	}, nil
}

func storageContext(ctx context.Context) meta.Context {
	return meta.WrapWithoutCancel(ctx, 1, UID, []uint32{GID})
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

// pin does not take the I/O lock. refs is atomic because attribute readers may
// release their pin while an I/O operation retains or closes an inode reference.
func (s *FS) pin(ino smb.Inode) (*inodeState, func()) {
	s.mu.Lock()
	st := s.inodes[ino]
	if st == nil {
		st = &inodeState{}
		s.inodes[ino] = st
	}
	st.users++
	s.mu.Unlock()
	return st, func() {
		s.mu.Lock()
		st.users--
		if st.users == 0 && st.refs.Load() == 0 {
			delete(s.inodes, ino)
		}
		s.mu.Unlock()
	}
}

func (s *FS) acquire(ino smb.Inode) (*inodeState, func()) {
	st, unpin := s.pin(ino)
	st.mu.Lock()
	return st, func() { st.mu.Unlock(); unpin() }
}

func (st *inodeState) snapshot() liveState {
	st.liveMu.RLock()
	defer st.liveMu.RUnlock()
	return st.live
}

func (st *inodeState) publish(live liveState) {
	st.liveMu.Lock()
	st.live = live
	st.liveMu.Unlock()
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

// Open retains a JuiceFS inode reference without creating or truncating data.
func (s *FS) Open(ctx context.Context, key smb.ObjectKey, access smb.Access) (smb.Handle, error) {
	if access & ^(smb.AccessRead|smb.AccessWrite) != 0 {
		return nil, smb.ErrInvalidParameter
	}
	if s.readOnly && access&smb.AccessWrite != 0 {
		return nil, smb.ErrReadOnly
	}
	st, release := s.acquire(key.Inode)
	defer release()
	a, err := s.attr(ctx, key, st)
	if err != nil {
		return nil, err
	}
	var streamData []byte
	if key.Stream != "" {
		streamData, err = s.stream(ctx, key)
		if err != nil {
			return nil, err
		}
	}
	if st.refs.Load() == 0 {
		var raw meta.Attr
		flags := uint32(syscall.O_RDWR)
		if s.readOnly {
			flags = syscall.O_RDONLY
		}
		if err = backendError(s.metadata.Open(storageContext(ctx), meta.Ino(key.Inode), flags, &raw)); err != nil {
			return nil, err
		}
		st.publish(liveState{size: raw.Length})
	}
	st.refs.Add(1)
	if key.Stream != "" {
		st.publishStream(key.Stream, streamData)
	}
	return &handle{owner: s, state: st, key: key, access: access, kind: a.Kind}, nil
}

func (s *FS) flush(ctx context.Context, st *inodeState) error {
	if st.writer != nil {
		if err := backendError(st.writer.Flush(storageContext(ctx))); err != nil {
			return errors.Join(err, ctx.Err())
		}
	}
	live := st.snapshot()
	live.dirty = false
	st.publish(live)
	return ctx.Err()
}

// Close always releases its reference, including when ctx is already canceled.
// Cancellation may abort the requested flush, but not native reference cleanup.
func (s *FS) Close(ctx context.Context, ref smb.Handle) error {
	cleanup := context.WithoutCancel(ctx)
	h, release, err := s.selected(cleanup, ref, false)
	if err != nil {
		return err
	}
	defer release()
	h.closed = true
	flushErr := s.flush(ctx, h.state)
	if h.state.refs.Add(-1) != 0 {
		return errors.Join(flushErr, ctx.Err())
	}
	var writerErr error
	if h.state.writer != nil {
		writerErr = backendError(h.state.writer.Close(storageContext(cleanup)))
		h.state.writer = nil
	}
	if h.state.reader != nil {
		h.state.reader.Close(storageContext(cleanup))
		h.state.reader = nil
	}
	closeErr := backendError(s.metadata.Close(storageContext(cleanup), meta.Ino(h.key.Inode)))
	return errors.Join(flushErr, writerErr, closeErr, ctx.Err())
}

// Flush covers the shared writer, then commits the requested metadata barrier.
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

// ReadAt reads the selected object, retaining the backend's shared-writer flush.
func (s *FS) ReadAt(ctx context.Context, ref smb.Handle, dst []byte, offset uint64) (int, error) {
	h, release, err := s.selected(ctx, ref, false)
	if err != nil {
		return 0, err
	}
	defer release()
	if h.access&smb.AccessRead == 0 {
		return 0, smb.ErrAccessDenied
	}
	if offset >= maxFileSize || uint64(len(dst)) >= maxFileSize-offset {
		return 0, smb.ErrFileTooLarge
	}
	if h.kind == smb.KindDirectory {
		return 0, smb.ErrIsDirectory
	}
	if len(dst) == 0 {
		return 0, nil
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
	if err = s.flush(ctx, h.state); err != nil {
		return 0, err
	}
	size := h.state.snapshot().size
	if offset >= size {
		return 0, io.EOF
	}
	requested := len(dst)
	if uint64(len(dst)) > size-offset {
		dst = dst[:size-offset]
	}
	if h.state.reader == nil {
		h.state.reader = s.reader.Open(meta.Ino(h.key.Inode), size)
	}
	var n int
	var eno syscall.Errno
	for {
		n, eno = h.state.reader.Read(storageContext(ctx), offset, dst)
		if eno != syscall.EAGAIN || ctx.Err() != nil {
			break
		}
	}
	if eno != 0 || ctx.Err() != nil {
		return n, errors.Join(backendError(eno), ctx.Err())
	}
	if n < requested {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt uses the kind and live length already retained by Open.
func (s *FS) WriteAt(ctx context.Context, ref smb.Handle, src []byte, offset uint64) (int, error) {
	h, release, err := s.selected(ctx, ref, true)
	if err != nil {
		return 0, err
	}
	defer release()
	if offset >= maxFileSize || uint64(len(src)) >= maxFileSize-offset {
		return 0, smb.ErrFileTooLarge
	}
	if h.kind == smb.KindDirectory {
		return 0, smb.ErrIsDirectory
	}
	if len(src) == 0 {
		return 0, nil
	}
	if h.key.Stream != "" {
		return s.writeStream(ctx, h.key, src, offset)
	}
	live := h.state.snapshot()
	if h.state.writer == nil {
		h.state.writer = s.writer.Open(meta.Ino(h.key.Inode), live.size, 0)
	}
	if err = backendError(h.state.writer.Write(storageContext(ctx), offset, src)); err != nil {
		return 0, errors.Join(err, ctx.Err())
	}
	live.size = max(live.size, offset+uint64(len(src)))
	live.modified = time.Now().UTC()
	live.dirty = true
	h.state.publish(live)
	return len(src), nil
}

func (s *FS) writeStream(ctx context.Context, key smb.ObjectKey, src []byte, offset uint64) (int, error) {
	end := offset + uint64(len(src))
	if end > maxStreamSize {
		return 0, smb.ErrFileTooLarge
	}
	data, err := s.stream(ctx, key)
	if err != nil {
		return 0, err
	}
	data = append([]byte{}, data...)
	if end > uint64(len(data)) {
		data = append(data, make([]byte, int(end)-len(data))...)
	}
	copy(data[offset:], src)
	if err = s.saveStream(ctx, key, data); err != nil {
		return 0, err
	}
	if err = s.touchStream(ctx, key.Inode); err != nil {
		return 0, err
	}
	return len(src), nil
}

func (s *FS) truncate(ctx context.Context, key smb.ObjectKey, st *inodeState, size uint64) error {
	if size >= maxFileSize {
		return smb.ErrFileTooLarge
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
		data = append([]byte{}, data...)
		if size > uint64(len(data)) {
			data = append(data, make([]byte, int(size)-len(data))...)
		} else {
			data = data[:size]
		}
		if err = s.saveStream(ctx, key, data); err != nil {
			return err
		}
		return s.touchStream(ctx, key.Inode)
	}
	if err = s.flush(ctx, st); err != nil {
		return err
	}
	// Nonzero flags mean truncation through an already retained JuiceFS inode.
	// This is the same distinction JuiceFS VFS makes for an open file in trash.
	var flags uint8
	if st.refs.Load() > 0 {
		flags = 1
	}
	if err = backendError(s.metadata.Truncate(storageContext(ctx), meta.Ino(key.Inode), flags, size, nil, false)); err != nil {
		return err
	}
	s.writer.Truncate(meta.Ino(key.Inode), size)
	s.reader.Truncate(meta.Ino(key.Inode), size)
	st.publish(liveState{size: size})
	s.filesystem.InvalidateAttr(meta.Ino(key.Inode))
	return nil
}

// Truncate changes only the selected object's length after draining its writer.
func (s *FS) Truncate(ctx context.Context, ref smb.Handle, size uint64) error {
	h, release, err := s.selected(ctx, ref, true)
	if err != nil {
		return err
	}
	defer release()
	return s.truncate(ctx, h.key, h.state, size)
}
