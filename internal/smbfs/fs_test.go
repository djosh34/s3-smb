package smbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
)

// testStore counts uploads. It can fail uploads, or hold uploads (slow) or
// downloads (cold) until resume is closed, signalling started first.
type testStore struct {
	object.ObjectStorage
	started chan struct{}
	resume  chan struct{}
	puts    atomic.Int64
	fail    atomic.Bool
	slow    atomic.Bool
	cold    atomic.Bool
}

func (s *testStore) hold(ctx context.Context) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *testStore) Put(ctx context.Context, key string, r io.Reader, getters ...object.AttrGetter) error {
	s.puts.Add(1)
	if s.slow.Load() {
		if err := s.hold(ctx); err != nil {
			return err
		}
	}
	if s.fail.Load() {
		return syscall.EIO
	}
	return s.ObjectStorage.Put(ctx, key, r, getters...)
}

func (s *testStore) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	if s.cold.Load() {
		if err := s.hold(ctx); err != nil {
			return nil, err
		}
	}
	return s.ObjectStorage.Get(ctx, key, off, limit, getters...)
}

// countedMetadata counts the metadata queries the adapter makes.
type countedMetadata struct {
	meta.Meta
	attrs       atomic.Int64
	xattrs      atomic.Int64
	lookups     atomic.Int64
	directories atomic.Int64
}

func (m *countedMetadata) GetAttr(ctx meta.Context, ino meta.Ino, a *meta.Attr) syscall.Errno {
	m.attrs.Add(1)
	return m.Meta.GetAttr(ctx, ino, a)
}

func (m *countedMetadata) GetXattr(ctx meta.Context, ino meta.Ino, key string, value *[]byte) syscall.Errno {
	m.xattrs.Add(1)
	return m.Meta.GetXattr(ctx, ino, key, value)
}

func (m *countedMetadata) Lookup(ctx meta.Context, parent meta.Ino, name string, ino *meta.Ino, a *meta.Attr, check bool) syscall.Errno {
	m.lookups.Add(1)
	return m.Meta.Lookup(ctx, parent, name, ino, a, check)
}

func (m *countedMetadata) Readdir(ctx meta.Context, ino meta.Ino, plus uint8, entries *[]*meta.Entry) syscall.Errno {
	m.directories.Add(1)
	return m.Meta.Readdir(ctx, ino, plus, entries)
}

type testBarrier struct {
	commit func(context.Context, bool) error
}

func (b testBarrier) Commit(ctx context.Context, full bool) error { return b.commit(ctx, full) }

type fixture struct {
	fs       *FS
	metadata meta.Meta
	native   *jfs.FileSystem
	store    *testStore
	config   *vfs.Config
	chunks   chunk.ChunkStore
	path     string
	handles  []smb.Handle
}

func newFixture(t *testing.T, capacity uint64) *fixture {
	t.Helper()
	return fixtureAt(t, t.TempDir(), capacity, true, 0)
}

func fixtureAt(t *testing.T, dir string, capacity uint64, initialize bool, trashDays int) *fixture {
	t.Helper()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	store := &testStore{ObjectStorage: blob, started: make(chan struct{}, 1), resume: make(chan struct{})}
	mc := meta.DefaultConf()
	mc.NoBGJob = true
	mc.MaxDeletes = 0
	mc.Retries = 0
	database := filepath.Join(dir, "meta.db")
	m, err := meta.NewSQLite(database, mc)
	if err != nil {
		t.Fatal(err)
	}
	format := meta.Format{Name: "smbfs-test", UUID: "smbfs-fixture", Storage: "file", BlockSize: 64, Compression: "none", Capacity: capacity, DirStats: true, TrashDays: trashDays}
	if initialize {
		if err = m.Init(&format, true); err != nil {
			t.Fatal(err)
		}
		root := meta.Attr{Uid: UID, Gid: GID, Mode: 0o700}
		if eno := m.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
			t.Fatal(eno)
		}
	} else {
		loaded, loadErr := m.Load(false)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		format = *loaded
	}
	if err = m.NewSession(true); err != nil {
		t.Fatal(err)
	}
	cc := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: 5 * time.Second, PutTimeout: time.Second}
	chunks := chunk.NewCachedStore(store, cc, nil)
	config := &vfs.Config{Meta: mc, Format: format, Chunk: &cc}
	native, err := jfs.NewFileSystem(config, m, chunks, nil)
	if err != nil {
		t.Fatal(err)
	}
	barrier, err := NewMetadataBarrier(database)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Options{Filesystem: native, Barrier: barrier, Capacity: capacity, MetadataPath: database, Config: config, Store: chunks})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{fs: adapter, metadata: m, native: native, store: store, path: database, config: config, chunks: chunks}
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

// readOnly returns a second, read-only adapter on the fixture's filesystem.
func (f *fixture) readOnly(t *testing.T) *FS {
	t.Helper()
	adapter, err := New(Options{Filesystem: f.native, Barrier: f.fs.barrier, ReadOnly: true, MetadataPath: f.path, Config: f.config, Store: f.chunks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adapter.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return adapter
}

func (f *fixture) cleanup(t *testing.T) {
	t.Helper()
	f.store.slow.Store(false)
	f.store.fail.Store(false)
	f.store.cold.Store(false)
	for _, h := range f.handles {
		if own, ok := h.(*handle); ok && own.closed {
			continue
		}
		if err := f.fs.Close(context.Background(), h); err != nil {
			t.Logf("close after fault: %v", err)
		}
	}
	if err := f.fs.Shutdown(); err != nil {
		t.Error(err)
	}
	if err := f.native.Close(); err != nil {
		t.Error(err)
	}
	if err := f.metadata.Shutdown(); err != nil {
		t.Error(err)
	}
}

func (f *fixture) create(t *testing.T, p string, kind smb.Kind) smb.Resolved {
	t.Helper()
	r, err := f.fs.Lookup(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	r, err = f.fs.Create(t.Context(), r.Name, kind)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) open(t *testing.T, key smb.ObjectKey, access smb.Access) smb.Handle {
	t.Helper()
	h, err := f.fs.Open(t.Context(), key, access)
	if err != nil {
		t.Fatal(err)
	}
	f.handles = append(f.handles, h)
	return h
}

func write(t *testing.T, s *FS, h smb.Handle, data string, offset uint64) {
	t.Helper()
	n, err := s.WriteAt(t.Context(), h, []byte(data), offset)
	if err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
}

func read(t *testing.T, s *FS, h smb.Handle, want []byte) {
	t.Helper()
	data := make([]byte, len(want)+1)
	n, err := s.ReadAt(t.Context(), h, data, 0)
	if !errors.Is(err, io.EOF) || !bytes.Equal(data[:n], want) {
		t.Fatalf("read = %q, %v; want %q, EOF", data[:n], err, want)
	}
}

func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v; want %v", err, want)
	}
}

// receive returns the next value from ch. Operations that block forever fail
// the test instead of hanging it.
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
	}
	t.Fatal("operation blocked")
	var zero T
	return zero
}

func TestIssue89FlushThroughNonWriterCommitsData(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	writer := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	other := f.open(t, r.Object, smb.AccessRead)
	write(t, f.fs, writer, "durable payload", 0)
	if err := f.fs.Flush(t.Context(), other, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	var a meta.Attr
	if eno := f.metadata.GetAttr(meta.Background(), meta.Ino(r.Object.Inode), &a); eno != 0 {
		t.Fatal(eno)
	}
	if a.Length != 15 || f.store.puts.Load() == 0 {
		t.Fatalf("committed size/puts = %d/%d", a.Length, f.store.puts.Load())
	}
	// A separate JuiceFS reference reads committed slices, not the live size.
	file, eno := f.native.Open(meta.Background(), "/data", vfs.MODE_MASK_R)
	if eno != 0 {
		t.Fatal(eno)
	}
	data := make([]byte, 15)
	n, err := file.Pread(meta.Background(), data, 0)
	if err != nil || string(data[:n]) != "durable payload" {
		t.Fatalf("committed read = %q, %v", data[:n], err)
	}
	if eno = file.Close(meta.Background()); eno != 0 {
		t.Fatal(eno)
	}
}

func TestFlushUploadErrorReachesNonWriter(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	writer := f.open(t, r.Object, smb.AccessWrite)
	other := f.open(t, r.Object, smb.AccessRead)
	f.store.fail.Store(true)
	write(t, f.fs, writer, "uncommitted", 0)
	requireError(t, f.fs.Flush(t.Context(), other, smb.SyncData), smb.ErrIO)
	if f.store.puts.Load() == 0 {
		t.Fatal("flush did not attempt an upload")
	}
}

func TestFlushMetadataBarrier(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	disk := f.fs.barrier
	var calls []bool
	f.fs.barrier = testBarrier{commit: func(ctx context.Context, full bool) error {
		if f.store.puts.Load() == 0 {
			t.Error("metadata barrier ran before the upload")
		}
		calls = append(calls, full)
		return disk.Commit(ctx, full)
	}}
	for _, key := range []smb.ObjectKey{base.Object, stream.Object} {
		h := f.open(t, key, smb.AccessRead|smb.AccessWrite)
		write(t, f.fs, h, "bytes", 0)
		for _, mode := range []smb.SyncMode{smb.SyncData, smb.SyncFull} {
			calls = nil
			if err := f.fs.Flush(t.Context(), h, mode); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[0] != (mode == smb.SyncFull) {
				t.Fatalf("mode %d: barrier calls %v", mode, calls)
			}
		}
	}
	failure := errors.New("metadata sync failed")
	f.fs.barrier = testBarrier{commit: func(context.Context, bool) error { return failure }}
	h := f.open(t, base.Object, smb.AccessWrite)
	err := f.fs.Flush(t.Context(), h, smb.SyncFull)
	requireError(t, err, failure)
	requireError(t, err, smb.ErrIO)
}

func TestCloseAlwaysReleasesReference(t *testing.T) {
	f := newFixture(t, 0)
	failing := f.create(t, "failing", smb.KindFile)
	h := f.open(t, failing.Object, smb.AccessWrite)
	f.store.fail.Store(true)
	write(t, f.fs, h, "data", 0)
	requireError(t, f.fs.Close(t.Context(), h), smb.ErrIO)
	requireError(t, f.fs.Close(t.Context(), h), smb.ErrInvalidHandle)
	f.store.fail.Store(false)

	canceled := f.create(t, "canceled", smb.KindFile)
	h = f.open(t, canceled.Object, smb.AccessWrite)
	write(t, f.fs, h, "data", 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireError(t, f.fs.Close(ctx, h), context.Canceled)
	requireError(t, f.fs.Close(t.Context(), h), smb.ErrInvalidHandle)
	if len(f.fs.inodes) != 0 {
		t.Fatal("closed reference retained")
	}
}

func TestIssue84TruncateCannotResurrectBufferedBytes(t *testing.T) {
	for _, size := range []uint64{0, 2} {
		t.Run(strconv.FormatUint(size, 10), func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			a := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
			b := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
			write(t, f.fs, a, "abcdefgh", 0)
			if err := f.fs.Truncate(t.Context(), b, size); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.Flush(t.Context(), a, smb.SyncData); err != nil {
				t.Fatal(err)
			}
			read(t, f.fs, b, []byte("abcdefgh")[:size])
			write(t, f.fs, a, "new", size)
			if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.Flush(t.Context(), a, smb.SyncFull); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.Close(t.Context(), a); err != nil {
				t.Fatal(err)
			}
			if err := f.fs.Close(t.Context(), b); err != nil {
				t.Fatal(err)
			}
			reopened := f.open(t, r.Object, smb.AccessRead)
			read(t, f.fs, reopened, []byte("abcdefgh")[:size])
		})
	}
}

func TestIssue113LookupAndDirectoryReportBufferedLength(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	puts := f.store.puts.Load()
	lookup, err := f.fs.Lookup(t.Context(), "data")
	if err != nil || lookup.Attr.Size != 6 {
		t.Fatalf("lookup = %+v, %v", lookup, err)
	}
	entries, err := f.fs.ReadDir(t.Context(), smb.Inode(meta.RootInode), 0, 10)
	if err != nil || len(entries) != 1 || entries[0].Attr.Size != 6 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	attr, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || attr.Size != 6 {
		t.Fatalf("attr = %+v, %v", attr, err)
	}
	if f.store.puts.Load() != puts {
		t.Fatal("attribute query uploaded buffered data")
	}
}

// Metadata queries use a separate lock from inode I/O, so a slow upload or a
// cold read must not block them, on the same inode or on others.
func TestIssue59MetadataQueriesDoNotWaitForInodeIO(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(map[bool]string{true: "upload", false: "cold_read"}[upload], func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			other := f.create(t, "other", smb.KindFile)
			h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
			otherHandle := f.open(t, other.Object, smb.AccessWrite)
			write(t, f.fs, h, "data", 0)
			slow := make(chan error, 1)
			if upload {
				f.store.slow.Store(true)
				go func() { slow <- f.fs.Flush(t.Context(), h, smb.SyncData) }()
			} else {
				if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
					t.Fatal(err)
				}
				f.store.cold.Store(true)
				go func() {
					_, err := f.fs.ReadAt(t.Context(), h, make([]byte, 4), 0)
					slow <- err
				}()
			}
			receive(t, f.store.started)
			queries := make(chan error, 1)
			go func() {
				_, err := f.fs.GetAttr(t.Context(), r.Object)
				if err == nil {
					_, err = f.fs.Lookup(t.Context(), "data")
				}
				if err == nil {
					_, err = f.fs.ReadDir(t.Context(), smb.Inode(meta.RootInode), 0, 10)
				}
				if err == nil {
					_, err = f.fs.WriteAt(t.Context(), otherHandle, []byte("other"), 0)
				}
				queries <- err
			}()
			if err := receive(t, queries); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-slow:
				t.Fatalf("I/O finished while S3 was held: %v", err)
			default:
			}
			f.store.slow.Store(false)
			f.store.cold.Store(false)
			close(f.store.resume)
			if err := receive(t, slow); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// flushAfterAttr flushes h right after the adapter reads inode's metadata
// attributes once.
type flushAfterAttr struct {
	meta.Meta
	flush func()
	inode meta.Ino
	armed atomic.Bool
}

func (m *flushAfterAttr) GetAttr(ctx meta.Context, ino meta.Ino, a *meta.Attr) syscall.Errno {
	eno := m.Meta.GetAttr(ctx, ino, a)
	if eno == 0 && ino == m.inode && m.armed.CompareAndSwap(true, false) {
		m.flush()
	}
	return eno
}

func TestAttributeLengthAcrossFlushCompletion(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	wrapped := &flushAfterAttr{Meta: f.metadata, inode: meta.Ino(r.Object.Inode), flush: func() {
		if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
			t.Error(err)
		}
	}}
	wrapped.armed.Store(true)
	f.fs.metadata = wrapped
	attr, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || attr.Size != 6 {
		t.Fatalf("attributes across flush = %+v, %v", attr, err)
	}
	if wrapped.armed.Load() {
		t.Fatal("flush race was not exercised")
	}
}

func TestIOUsesRetainedKindAndLength(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	counted := &countedMetadata{Meta: f.metadata}
	f.fs.metadata = counted
	for range 5 {
		write(t, f.fs, h, "data", 0)
		read(t, f.fs, h, []byte("data"))
	}
	if counted.attrs.Load() != 0 || counted.xattrs.Load() != 0 {
		t.Fatalf("adapter queried attributes during I/O: attr=%d, xattr=%d", counted.attrs.Load(), counted.xattrs.Load())
	}
}

func TestBaseOffsetsAndZeroFilledExtensions(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	other := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "abc", 4)
	read(t, f.fs, other, []byte{0, 0, 0, 0, 'a', 'b', 'c'})
	if err := f.fs.Truncate(t.Context(), other, 10); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a', 'b', 'c', 0, 0, 0})
}

func requireNoNativeLocks(t *testing.T, f *fixture) {
	t.Helper()
	for _, query := range []string{"SELECT COUNT(*) FROM jfs_plock", "SELECT COUNT(*) FROM jfs_flock"} {
		var count int
		if err := f.fs.directory.QueryRowContext(t.Context(), query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s: %d JuiceFS lock rows", query, count)
		}
	}
}

// Storage references carry no SMB share or lock policy and never take JuiceFS
// locks, which would survive a crash in the metadata database (#101).
func TestIssue124ReferencesHaveNoOpenOrLockPolicy(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	for _, key := range []smb.ObjectKey{base.Object, stream.Object} {
		a := f.open(t, key, smb.AccessRead|smb.AccessWrite)
		b := f.open(t, key, smb.AccessRead|smb.AccessWrite)
		write(t, f.fs, a, "first", 0)
		write(t, f.fs, b, "other", 0)
		if err := f.fs.Flush(t.Context(), b, smb.SyncFull); err != nil {
			t.Fatal(err)
		}
		if err := f.fs.Truncate(t.Context(), a, 3); err != nil {
			t.Fatal(err)
		}
		requireNoNativeLocks(t, f)
		if err := f.fs.Close(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		requireError(t, f.fs.Close(t.Context(), a), smb.ErrInvalidHandle)
		read(t, f.fs, b, []byte("oth"))
		if err := f.fs.Close(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	}
	requireNoNativeLocks(t, f)
	if len(f.fs.inodes) != 0 || len(f.fs.parents) != 0 {
		t.Fatal("unused coherence state retained")
	}
}

func TestConcurrentInodeWritesAndQueries(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	a := f.open(t, r.Object, smb.AccessWrite)
	b := f.open(t, r.Object, smb.AccessWrite)
	var group sync.WaitGroup
	errs := make(chan error, 3)
	for _, w := range []struct {
		h      smb.Handle
		fill   string
		offset uint64
	}{{a, "a", 0}, {b, "b", 100}} {
		group.Go(func() {
			for i := range uint64(10) {
				if _, err := f.fs.WriteAt(t.Context(), w.h, bytes.Repeat([]byte(w.fill), 10), w.offset+i*10); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	group.Go(func() {
		for range 20 {
			if _, err := f.fs.GetAttr(t.Context(), r.Object); err != nil {
				errs <- err
				return
			}
		}
	})
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	attr, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || attr.Size != 200 {
		t.Fatalf("size = %d, %v", attr.Size, err)
	}
}

func TestAccessAndCancellation(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	reader := f.open(t, r.Object, smb.AccessRead)
	_, err := f.fs.WriteAt(t.Context(), reader, []byte("x"), 0)
	requireError(t, err, smb.ErrAccessDenied)
	writer := f.open(t, r.Object, smb.AccessWrite)
	_, err = f.fs.ReadAt(t.Context(), writer, make([]byte, 1), 0)
	requireError(t, err, smb.ErrAccessDenied)
	requireError(t, f.fs.Flush(t.Context(), writer, smb.SyncMode(9)), smb.ErrInvalidParameter)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.fs.Lookup(ctx, "data")
	requireError(t, err, context.Canceled)
	_, err = f.fs.WriteAt(ctx, writer, []byte("x"), 0)
	requireError(t, err, context.Canceled)
}

func TestFileSizeBoundary(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "abc", 0)
	puts := f.store.puts.Load()
	for _, size := range []uint64{maxFileSize, maxFileSize + 1, math.MaxUint64} {
		requireError(t, f.fs.Truncate(t.Context(), h, size), smb.ErrFileTooLarge)
		requireError(t, f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}), smb.ErrFileTooLarge)
		_, err := f.fs.WriteAt(t.Context(), h, []byte("x"), size-1)
		requireError(t, err, smb.ErrFileTooLarge)
		n, err := f.fs.ReadAt(t.Context(), h, make([]byte, 1), size-1)
		if n != 0 || smb.StatusFromError(err) != smb.StatusEndOfFile {
			t.Fatalf("read at %d = %d, %v", size-1, n, err)
		}
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != 3 || f.store.puts.Load() != puts {
		t.Fatalf("invalid range mutated data: %+v, %v, puts=%d", a, err, f.store.puts.Load())
	}
	if err = f.fs.Truncate(t.Context(), h, maxFileSize-1); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, h, "z", maxFileSize-2)
	data := make([]byte, 2)
	n, err := f.fs.ReadAt(t.Context(), h, data, maxFileSize-2)
	if n != 1 || data[0] != 'z' || !errors.Is(err, io.EOF) {
		t.Fatalf("read across the last byte = %q, %d, %v", data, n, err)
	}
	if err = f.fs.Truncate(t.Context(), h, 3); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte("abc"))

	stream := f.create(t, "data:fork", smb.KindFile)
	sh := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, sh, "data", 0)
	for _, offset := range []uint64{4, math.MaxUint64} {
		n, err = f.fs.ReadAt(t.Context(), sh, make([]byte, 1), offset)
		if n != 0 || smb.StatusFromError(err) != smb.StatusEndOfFile {
			t.Fatalf("stream read at %d = %d, %v", offset, n, err)
		}
	}
}
