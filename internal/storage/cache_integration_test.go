// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMinIOCacheEvictionRefetch(t *testing.T) {
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("needs MinIO: run scripts/check.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := object.NewS3(object.S3Options{
		Bucket: fmt.Sprintf("cache-eviction-%d", time.Now().UnixNano()), Region: "us-east-1",
		Endpoint: endpoint, AccessKey: "s3smb-test-access", SecretKey: "s3smb-test-secret-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := raw.(io.Closer); ok {
		t.Cleanup(func() {
			if err := closer.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err := raw.Create(ctx); err != nil {
		t.Fatal(err)
	}
	format, err := NewFormat("cache-eviction", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	const payloadSize = 1 << 20
	capacity := int64(2 << 20)
	conf, err := CacheConfig(format, t.TempDir(), &capacity)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countStore{ObjectStorage: raw}
	registry := prometheus.NewRegistry()
	store := chunk.NewCachedStore(counted, conf, registry)

	// Each partial block is cached on write. Finish waits for its S3 upload,
	// unlike writeback mode. Eight blocks exceed both the capacity and slack.
	data := make([][]byte, 8)
	for i := range data {
		data[i] = make([]byte, payloadSize)
		for offset := range data[i] {
			data[i][offset] = byte((offset*31 + offset/251 + i*17) % 256)
		}
		writer := store.NewWriter(uint64(i+1), 0)
		n, err := writer.WriteAt(data[i], 0)
		if err != nil || n != payloadSize {
			writer.Abort()
			t.Fatalf("write slice %d: bytes=%d err=%v", i+1, n, err)
		}
		if err := writer.Finish(payloadSize); err != nil {
			writer.Abort()
			t.Fatalf("flush slice %d: %v", i+1, err)
		}
		waitDiskCache(t, store, conf.CacheDir, capacity, payloadSize)
		if i == 0 {
			if !sliceCached(t, store, 1, payloadSize) {
				t.Fatal("first slice did not populate the disk cache")
			}
			before := counted.gets.Load()
			readCacheSlice(t, ctx, store, 1, data[0])
			if counted.gets.Load() != before {
				t.Fatal("warm read fetched from S3 instead of the disk cache")
			}
		}
	}
	objects, _, _, err := raw.List(ctx, "chunks/", "", "", "", 100, true)
	if err != nil {
		t.Fatal(err)
	}
	var remoteBytes int64
	for _, obj := range objects {
		remoteBytes += obj.Size()
	}
	if remoteBytes != int64(len(data)*payloadSize) {
		t.Fatalf("S3 dataset size = %d, want %d", remoteBytes, len(data)*payloadSize)
	}
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var evictions float64
	for _, metric := range metrics {
		if metric.GetName() == "blockcache_evicts" {
			for _, sample := range metric.GetMetric() {
				evictions += sample.GetCounter().GetValue()
			}
		}
	}
	if evictions == 0 {
		t.Fatal("dataset did not force disk cache eviction")
	}

	// Keep the same store and directory. Missing slices must reach MinIO,
	// not a fresh cache or a retained in-memory page.
	refetched := 0
	for i, want := range data {
		id := uint64(i + 1)
		cached := sliceCached(t, store, id, payloadSize)
		before := counted.gets.Load()
		readCacheSlice(t, ctx, store, id, want)
		if !cached {
			if counted.gets.Load() <= before {
				t.Fatalf("evicted slice %d was not refetched from S3", id)
			}
			refetched++
		}
		waitDiskCache(t, store, conf.CacheDir, capacity, payloadSize)
	}
	if refetched == 0 {
		t.Fatal("no evicted data was read back")
	}
	t.Logf("verified %d bytes, refetched %d slices after %.0f evictions", remoteBytes, refetched, evictions)
}

func sliceCached(t *testing.T, store chunk.ChunkStore, id uint64, size uint32) bool {
	t.Helper()
	cached := false
	if err := store.CheckCache(id, size, func(exists bool, _ string, _ int) { cached = exists }); err != nil {
		t.Fatal(err)
	}
	return cached
}

func readCacheSlice(t *testing.T, ctx context.Context, store chunk.ChunkStore, id uint64, want []byte) {
	t.Helper()
	page := chunk.NewOffPage(len(want))
	defer page.Release()
	n, err := store.NewReader(id, len(want)).ReadAt(ctx, page, 0)
	if err != nil || n != len(want) {
		t.Fatalf("read slice %d: bytes=%d err=%v", id, n, err)
	}
	if !bytes.Equal(page.Data, want) {
		t.Fatalf("slice %d changed bytes", id)
	}
}

func waitDiskCache(t *testing.T, store chunk.ChunkStore, dir string, capacity int64, blockSize int) {
	t.Helper()
	// JuiceFS disk_cache.go writes one block before cleanupFull evicts to
	// 95% of capacity. Allow that block while flushing, plus 4 KiB for the
	// lock and checksum overhead. Once pending pages drain, allow only 4 KiB.
	// Measure all regular files, including checksums and temporary files.
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending := store.UsedMemory()
		var size int64
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil // An eviction can remove a file during the walk.
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			size += info.Size()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if size > capacity+int64(blockSize)+4096 {
			t.Fatalf("cache directory uses %d bytes, exceeds %d bytes with flush slack", size, capacity+int64(blockSize)+4096)
		}
		if pending == 0 {
			if size > capacity+4096 {
				t.Fatalf("settled cache directory uses %d bytes, exceeds %d bytes", size, capacity+4096)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("disk cache did not settle: pending=%d bytes, directory=%d bytes", pending, size)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
