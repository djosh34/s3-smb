// SPDX-License-Identifier: AGPL-3.0-only
// Modified for s3-smb, 2026. See docs/vendored.md.

package chunk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// Adapted from swarm commit 0db3360. One caller writes a partial block with
// compression disabled while the real disk cache flushes it in the background.
func TestCachedStoreUploadPublication(t *testing.T) {
	backing, err := object.CreateStorage("mem", t.Name(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	conf := Config{
		BlockSize: 4 << 20, BufferSize: 32 << 20, Compress: "none",
		MaxUpload: 1, MaxDownload: 1, MaxRetries: 1,
		GetTimeout: time.Second, PutTimeout: time.Second,
		CacheDir: filepath.Join(t.TempDir(), "cache"), CacheSize: 4 << 20,
		CacheMode: 0600, CacheChecksum: CsExtend, CacheScanInterval: time.Hour,
		FreeSpace: 0.001, AutoCreate: true, CacheFullBlock: true,
	}
	conf.SelfCheck("publication-test")
	store := NewCachedStore(backing, conf, nil).(*cachedStore)
	cacheManager, ok := store.bcache.(*cacheManager)
	if !ok {
		t.Fatal("disk cache unexpectedly fell back to memory")
	}
	t.Cleanup(func() { waitPublicationCache(t, store) })

	src := make([]byte, 521)
	for i := range src {
		src[i] = byte(i*17 + 18)
	}
	writer := store.NewWriter(1, 0)
	if n, err := writer.WriteAt(src, 0); err != nil || n != len(src) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if err := writer.Finish(len(src)); err != nil {
		t.Fatal(err)
	}
	waitPublicationCache(t, store)

	key := sliceForRead(1, len(src), store).key(0)
	cache := cacheManager.getStore(key)
	if cache == nil {
		t.Fatal("disk cache unavailable")
	}
	if _, err := os.Stat(cache.cachePath(key)); err != nil {
		t.Fatalf("block was not flushed to disk cache: %v", err)
	}
	page := NewPage(make([]byte, len(src)))
	defer page.Release()
	n, err := store.NewReader(1, len(src)).ReadAt(context.Background(), page, 0)
	if err != nil || n != len(src) || !bytes.Equal(page.Data, src) {
		t.Fatalf("read: n=%d err=%v outputMatches=%v", n, err, bytes.Equal(page.Data, src))
	}
}

func waitPublicationCache(t *testing.T, store *cachedStore) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for store.bcache.usedMemory() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.bcache.usedMemory() != 0 {
		t.Fatal("disk cache did not drain")
	}
}
