package smbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
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

type testStore struct {
	object.ObjectStorage
	started chan struct{}
	resume  chan struct{}
	puts    atomic.Int64
	fail    atomic.Bool
	slow    atomic.Bool
	cold    atomic.Bool
}

func (s *testStore) Put(ctx context.Context, key string, r io.Reader, getters ...object.AttrGetter) error {
	s.puts.Add(1)
	if s.slow.Load() {
		select {
		case s.started <- struct{}{}:
		default:
		}
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.fail.Load() {
		return syscall.EIO
	}
	return s.ObjectStorage.Put(ctx, key, r, getters...)
}

func (s *testStore) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	if s.cold.Load() {
		select {
		case s.started <- struct{}{}:
		default:
		}
		select {
		case <-s.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.ObjectStorage.Get(ctx, key, off, limit, getters...)
}

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

func (f *fixture) cleanup(t *testing.T) {
	t.Helper()
	f.store.slow.Store(false)
	f.store.fail.Store(false)
	f.store.cold.Store(false)
	for _, h := range f.handles {
		nativeHandle, ok := h.(*handle)
		if !ok {
			t.Error("unexpected handle type")
			continue
		}
		if !nativeHandle.closed {
			if err := f.fs.Close(context.Background(), h); err != nil {
				t.Logf("close after fault: %v", err)
			}
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
	// A separate native reference reads from committed slices, not our live size.
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

type testBarrier struct {
	commit func(context.Context, bool) error
}

func (b testBarrier) Commit(ctx context.Context, full bool) error { return b.commit(ctx, full) }

func TestFlushMetadataBarrierError(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	failure := errors.New("metadata sync failed")
	f.fs.barrier = testBarrier{commit: func(_ context.Context, full bool) error {
		if !full || f.store.puts.Load() == 0 {
			t.Error("barrier ran before upload, or not full")
		}
		return failure
	}}
	write(t, f.fs, h, "data", 0)
	err := f.fs.Flush(t.Context(), h, smb.SyncFull)
	requireError(t, err, failure)
	requireError(t, err, smb.ErrIO)
}

func TestFullSyncWaitsForBarrier(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	started := make(chan bool, 1)
	resume := make(chan struct{})
	realBarrier := f.fs.barrier
	f.fs.barrier = testBarrier{commit: func(ctx context.Context, full bool) error {
		started <- full
		select {
		case <-resume:
			return realBarrier.Commit(ctx, full)
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	write(t, f.fs, h, "data", 0)
	done := make(chan error, 1)
	go func() { done <- f.fs.Flush(t.Context(), h, smb.SyncFull) }()
	select {
	case full := <-started:
		if !full {
			t.Error("not a full barrier")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("barrier not reached")
	}
	select {
	case err := <-done:
		t.Fatalf("flush returned before barrier: %v", err)
	default:
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
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

func TestIssue96ExplicitTimestampSurvivesFlush(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	writer := f.open(t, r.Object, smb.AccessWrite)
	other := f.open(t, r.Object, smb.AccessRead)
	write(t, f.fs, writer, "dirty", 0)
	stamp := time.Unix(1000000000, 123456700).UTC()
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Flush(t.Context(), other, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Modified.Equal(stamp) {
		t.Fatalf("mtime = %v, %v; want %v", a.Modified, err, stamp)
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

func TestIssue59OtherInodesProgressDuringSlowFlush(t *testing.T) {
	f := newFixture(t, 0)
	slow := f.create(t, "slow", smb.KindFile)
	other := f.create(t, "other", smb.KindFile)
	h := f.open(t, slow.Object, smb.AccessWrite)
	otherHandle := f.open(t, other.Object, smb.AccessRead|smb.AccessWrite)
	f.store.slow.Store(true)
	write(t, f.fs, h, "slow upload", 0)
	done := make(chan error, 1)
	go func() { done <- f.fs.Flush(t.Context(), h, smb.SyncData) }()
	select {
	case <-f.store.started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	progressed := make(chan error, 1)
	go func() {
		_, err := f.fs.GetAttr(t.Context(), other.Object)
		if err == nil {
			_, err = f.fs.WriteAt(t.Context(), otherHandle, []byte("other"), 0)
		}
		if err == nil {
			_, err = f.fs.Lookup(t.Context(), "other")
		}
		if err == nil {
			_, err = f.fs.GetAttr(t.Context(), slow.Object)
		}
		if err == nil {
			_, err = f.fs.Lookup(t.Context(), "slow")
		}
		if err == nil {
			_, err = f.fs.ReadDir(t.Context(), 1, 0, 10)
		}
		progressed <- err
	}()
	select {
	case err := <-progressed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("unrelated inode blocked")
	}
	f.store.slow.Store(false)
	close(f.store.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
