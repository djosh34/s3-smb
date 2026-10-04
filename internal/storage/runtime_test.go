// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func TestCacheConfigSize(t *testing.T) {
	f, err := NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	c, err := CacheConfig(f, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.CacheSize != 100<<30 {
		t.Fatalf("default cache size = %d", c.CacheSize)
	}
	size := uint64(1000000)
	c, err = CacheConfig(f, t.TempDir(), &size)
	if err != nil {
		t.Fatal(err)
	}
	if c.CacheSize != size {
		t.Fatalf("cache size = %d, want %d", c.CacheSize, size)
	}
}

// The budgets follow vfs/writer.go flush, vfs/reader.go retry_time and
// chunk/cached_store.go upload, ignoring request time.
func TestDataPathRetryBudgetCoversOutage(t *testing.T) {
	format, err := NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	var zero uint64
	c, err := CacheConfig(format, "/unusable", &zero)
	if err != nil {
		t.Fatal(err)
	}
	conf := filesystemConfig(format, &c)
	flush := max(time.Duration((conf.Meta.Retries+2)*(conf.Meta.Retries+2)/2)*time.Second, 5*time.Minute)
	var download, upload time.Duration
	for attempt := 1; attempt <= conf.Meta.Retries; attempt++ {
		if attempt < 30 {
			download += time.Duration((attempt-1)*300+1) * time.Millisecond
		} else {
			download += 10 * time.Second
		}
	}
	for attempt := 0; attempt <= conf.Chunk.MaxRetries; attempt++ {
		upload += time.Duration(attempt*attempt) * time.Second
	}
	for name, budget := range map[string]time.Duration{"flush": flush, "download": download, "upload": upload} {
		if budget < 6*time.Minute {
			t.Errorf("%s budget %s must cover a 300s outage with at least 60s margin", name, budget)
		}
	}
	if flush <= upload+conf.Chunk.PutTimeout {
		t.Fatal("flush deadline must allow the last upload attempt to finish")
	}
}

type countStore struct {
	object.ObjectStorage
	gets atomic.Int64
}

func (s *countStore) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	s.gets.Add(1)
	return s.ObjectStorage.Get(ctx, key, off, limit, getters...)
}

func readSlice(store chunk.ChunkStore, id uint64, size int) ([]byte, error) {
	page := chunk.NewOffPage(size)
	defer page.Release()
	n, err := store.NewReader(id, size).ReadAt(context.Background(), page, 0)
	return bytes.Clone(page.Data[:n]), err
}

func TestZeroCacheColdRead(t *testing.T) {
	raw, err := object.CreateStorage("file", t.TempDir()+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countStore{ObjectStorage: raw}
	f, err := NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	// A zero cache must not touch a configured cache path.
	cache := filepath.Join(t.TempDir(), "old-cache")
	if err = os.WriteFile(cache, []byte("not a cache directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var zero uint64
	c, err := CacheConfig(f, filepath.Join(cache, "unusable"), &zero)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("cold remote fixture"), 30000)
	first := chunk.NewCachedStore(counted, c, nil)
	w := first.NewWriter(17, 0)
	if n, writeErr := w.WriteAt(data, 0); writeErr != nil || n != len(data) {
		t.Fatal(n, writeErr)
	}
	if err = w.Finish(len(data)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		cold := chunk.NewCachedStore(counted, c, nil)
		before := counted.gets.Load()
		got, readErr := readSlice(cold, 17, len(data))
		if readErr != nil || !bytes.Equal(got, data) {
			t.Fatal("cold read mismatch", readErr)
		}
		if counted.gets.Load() <= before {
			t.Fatal("zero cache read did not reach remote")
		}
		if cold.UsedMemory() != 0 {
			t.Fatal("retained memory cache at zero")
		}
	}
	old, err := os.ReadFile(filepath.Clean(cache))
	if err != nil || string(old) != "not a cache directory" {
		t.Fatal("old cache touched", err)
	}
	// Referenced but missing data must fail, not read as zeroes.
	if err = first.Remove(17, len(data)); err != nil {
		t.Fatal(err)
	}
	if _, err = readSlice(chunk.NewCachedStore(counted, c, nil), 17, len(data)); err == nil {
		t.Fatal("missing data reported success")
	}
}

type testRuntime struct {
	*Runtime
	meta meta.Meta
}

func (r testRuntime) close() error { return errors.Join(r.Close(), r.meta.Shutdown()) }

// openRuntime opens the metadata at path and a filesystem without a local cache.
// It initializes new metadata with format, or loads existing metadata.
func openRuntime(t *testing.T, path string, raw object.ObjectStorage, format *meta.Format, initialize bool) testRuntime {
	t.Helper()
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	m, err := OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	if initialize {
		err = m.Init(format, false)
	} else {
		_, err = m.Load(true)
	}
	if err != nil {
		t.Fatal(errors.Join(err, m.Shutdown()))
	}
	var zero uint64
	runtime, err := OpenFilesystem(m, raw, format, t.TempDir(), &zero, func() error { return nil })
	if err != nil {
		t.Fatal(errors.Join(err, m.Shutdown()))
	}
	r := testRuntime{Runtime: runtime, meta: m}
	if err = m.NewSession(false); err != nil {
		t.Fatal(errors.Join(err, r.close()))
	}
	return r
}

func TestFilesystemRestart(t *testing.T) {
	f, err := NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := object.CreateStorage("file", t.TempDir()+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "metadata.db")
	runtime := openRuntime(t, path, raw, f, true)
	if runtime.Config.Format.UUID != f.UUID || runtime.Config.Chunk.CacheSize != 0 || runtime.Config.Meta.Retries != filesystemRetries {
		t.Fatal("runtime did not expose its I/O settings")
	}
	ctx := meta.NewContext(1, 0, []uint32{0})
	handle, errno := runtime.FS.Create(ctx, "/fixture", 0o600, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	data := []byte("durable data")
	if n, writeErr := handle.Write(ctx, data); writeErr != 0 || n != len(data) {
		t.Fatal(n, writeErr)
	}
	if errno = handle.Fsync(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if errno = handle.Close(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if err = runtime.close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("new SQLite mode %o", info.Mode().Perm())
	}

	runtime = openRuntime(t, path, raw, f, false)
	t.Cleanup(func() {
		if closeErr := runtime.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	handle, errno = runtime.FS.Open(ctx, "/fixture", 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	buf := make([]byte, len(data))
	n, err := handle.Read(ctx, buf)
	if err != nil || n != len(data) || !bytes.Equal(buf, data) {
		t.Fatal("restart read mismatch", n, err)
	}
	if errno = handle.Close(ctx); errno != 0 {
		t.Fatal(errno)
	}
}

func TestFilesystemCapacity(t *testing.T) {
	for _, capacity := range []uint64{16 * 1024, 0} {
		format, err := NewFormat("test", false, 14)
		if err != nil {
			t.Fatal(err)
		}
		format.Capacity = capacity
		raw, err := object.CreateStorage("file", t.TempDir()+"/", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		runtime := openRuntime(t, filepath.Join(t.TempDir(), "metadata.db"), raw, format, true)
		below, errno := writeTwice(runtime, 4096, 16*1024)
		if below != 0 {
			t.Errorf("write below capacity %d failed: %v", capacity, below)
		}
		if capacity == 0 && errno != 0 {
			t.Errorf("unlimited write failed: %v", errno)
		}
		if capacity != 0 && !errors.Is(errno, syscall.ENOSPC) {
			t.Errorf("write beyond capacity: got %v, want ENOSPC", errno)
		}
		if err = runtime.close(); err != nil {
			t.Fatal(err)
		}
	}
}

// writeTwice writes and syncs first bytes, then second bytes, and returns the
// error of each sync. JuiceFS checks capacity when it commits writes.
func writeTwice(runtime testRuntime, first, second int) (syscall.Errno, syscall.Errno) {
	ctx := meta.NewContext(1, 0, []uint32{0})
	handle, errno := runtime.FS.Create(ctx, "/fixture", 0o600, 0)
	if errno != 0 {
		return errno, 0
	}
	write := func(data []byte) syscall.Errno {
		if _, writeErr := handle.Write(ctx, data); writeErr != 0 {
			return writeErr
		}
		return handle.Fsync(ctx)
	}
	firstErr := write(bytes.Repeat([]byte("a"), first))
	secondErr := write(bytes.Repeat([]byte("b"), second))
	if closeErr := handle.Close(ctx); secondErr == 0 {
		secondErr = closeErr
	}
	return firstErr, secondErr
}

func TestDeletionProtectionAtExecution(t *testing.T) {
	raw := memory(t)
	ctx := context.Background()
	if err := raw.Put(ctx, "keep", bytes.NewReader([]byte("data"))); err != nil {
		t.Fatal(err)
	}
	expired := errors.New("expired protection")
	s := &maintenanceStore{raw, func() error { return expired }}
	if err := s.Delete(ctx, "keep"); !errors.Is(err, expired) {
		t.Fatal(err)
	}
	if _, err := raw.Head(ctx, "keep"); err != nil {
		t.Fatal("protected data deleted", err)
	}
}
