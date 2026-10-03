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
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func TestNewFormatUsesNoCompression(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		f, err := NewFormat("test", encrypted, 14)
		if err != nil {
			t.Fatal(err)
		}
		if f.Compression != "none" {
			t.Fatalf("encrypted=%t: new format compression = %q, want none", encrypted, f.Compression)
		}
		c, err := CacheConfig(f, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.Compress != "none" {
			t.Fatalf("encrypted=%t: chunk compression = %q, want none", encrypted, c.Compress)
		}
	}
}

func TestCacheConfig(t *testing.T) {
	f, err := NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	c, err := CacheConfig(f, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.CacheSize != 100<<30 {
		t.Fatal("native omitted capacity changed")
	}
	for _, size := range []int64{0, 1, 999999, 1000000, 1000000000} {
		c, err = CacheConfig(f, "/not/usable", &size)
		if err != nil {
			t.Fatal(err)
		}
		if c.CacheSize != uint64(size) {
			t.Fatal("decimal capacity truncated")
		}
		if size == 0 && (c.CacheDir != "memory" || c.Prefetch != 0 || c.Writeback) {
			t.Fatal("zero retained cache not disabled")
		}
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
func TestZeroCacheColdRead(t *testing.T) {
	remote := t.TempDir()
	raw, err := object.CreateStorage("file", remote, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countStore{ObjectStorage: raw}
	f, _ := NewFormat("test", false, 14)
	zero := int64(0)
	cache := filepath.Join(t.TempDir(), "old-cache")
	if err = os.WriteFile(cache, []byte("not a cache directory"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := CacheConfig(f, filepath.Join(cache, "unusable"), &zero)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("cold remote fixture"), 30000)
	first := chunk.NewCachedStore(counted, c, nil)
	w := first.NewWriter(17, 0)
	if n, err := w.WriteAt(data, 0); err != nil || n != len(data) {
		t.Fatal(n, err)
	}
	if err = w.Finish(len(data)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cold := chunk.NewCachedStore(counted, c, nil)
		page := chunk.NewOffPage(len(data))
		before := counted.gets.Load()
		n, err := cold.NewReader(17, len(data)).ReadAt(context.Background(), page, 0)
		if err != nil || n != len(data) || !bytes.Equal(page.Data, data) {
			page.Release()
			t.Fatal("cold read mismatch", n, err)
		}
		page.Release()
		if counted.gets.Load() <= before {
			t.Fatal("zero cache read did not reach remote")
		}
		if cold.UsedMemory() != 0 {
			t.Fatal("retained memory cache at zero")
		}
	}
	old, err := os.ReadFile(cache)
	if err != nil || string(old) != "not a cache directory" {
		t.Fatal("old cache touched", err)
	}
	// Referenced but missing native data must fail, not become zero-filled data.
	if err = first.Remove(17, len(data)); err != nil {
		t.Fatal(err)
	}
	cold := chunk.NewCachedStore(counted, c, nil)
	page := chunk.NewOffPage(len(data))
	defer page.Release()
	if _, err = cold.NewReader(17, len(data)).ReadAt(context.Background(), page, 0); err == nil {
		t.Fatal("missing data reported success")
	}
}
func TestFilesystemNativeLifecycle(t *testing.T) {
	f, err := NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	path := filepath.Join(t.TempDir(), "metadata.db")
	m, err := OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Init(f, false); err != nil {
		t.Fatal(err)
	}
	if err = m.NewSession(false); err != nil {
		t.Fatal(err)
	}
	raw, err := object.CreateStorage("file", t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	runtime, err := OpenFilesystem(m, raw, f, "/unusable", &zero, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx := meta.NewContext(1, 0, []uint32{0})
	handle, errno := runtime.FS.Create(ctx, "/fixture", 0600, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	data := []byte("native filesystem durable data")
	if n, errno := handle.Write(ctx, data); errno != 0 || n != len(data) {
		t.Fatal(n, errno)
	}
	if errno = handle.Fsync(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if errno = handle.Close(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if err = runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err = m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("new SQLite mode %o", info.Mode().Perm())
	}
	// Existing readable modes are preserved, not rejected or silently chmodded.
	if err = os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Load(true); err != nil {
		t.Fatal(err)
	}
	if err = m.NewSession(false); err != nil {
		t.Fatal(err)
	}
	runtime, err = OpenFilesystem(m, raw, f, "/unusable", &zero, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close(); _ = m.Shutdown() }()
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
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatal("existing file mode changed")
	}
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
