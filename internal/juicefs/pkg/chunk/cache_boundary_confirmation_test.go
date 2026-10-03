// SPDX-License-Identifier: AGPL-3.0-only
package chunk

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func confirmationStore(t *testing.T, codec string, diskCache bool) *cachedStore {
	t.Helper()
	backing, err := object.CreateStorage("mem", t.Name(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cc := Config{BlockSize: 4 << 20, BufferSize: 32 << 20, Compress: codec,
		MaxUpload: 1, MaxDownload: 1, MaxRetries: 1,
		GetTimeout: time.Second, PutTimeout: time.Second}
	if diskCache {
		cc.CacheDir = filepath.Join(t.TempDir(), "cache")
		cc.CacheSize = 4 << 20
		cc.CacheMode = 0600
		cc.CacheChecksum = CsExtend
		cc.CacheScanInterval = time.Hour
		cc.FreeSpace = 0.001
		cc.AutoCreate = true
		cc.CacheFullBlock = true
	}
	cc.SelfCheck("confirmation")
	store := NewCachedStore(backing, cc, nil).(*cachedStore)
	if diskCache {
		if _, ok := store.bcache.(*cacheManager); !ok {
			t.Fatal("disk cache unexpectedly fell back to memory")
		}
		// Let an already queued cache flush finish before removing its directory.
		t.Cleanup(func() {
			deadline := time.Now().Add(3 * time.Second)
			for store.bcache.usedMemory() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if store.bcache.usedMemory() != 0 {
				t.Error("disk cache did not drain")
			}
		})
	}
	return store
}

func confirmationData() []byte {
	src := make([]byte, 521)
	for i := range src {
		src[i] = byte(i*17 + 18)
	}
	return src
}

func confirmationWrite(t *testing.T, store *cachedStore, src []byte) {
	t.Helper()
	w := store.NewWriter(1, 0)
	if n, err := w.WriteAt(src, 0); err != nil || n != len(src) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if err := w.Finish(len(src)); err != nil {
		t.Fatal(err)
	}
}

// Direct chunk-store integration: no SQLite, SMB filesystem fixture, Page pool
// churn, injected delay, or concurrent caller. The reader takes a destination
// sub-page as the production VFS reader does for one slice of a larger read.
func TestCachedStoreReadDestinationBoundary(t *testing.T) {
	for _, codec := range []string{"none", "lz4", "zstd"} {
		t.Run(codec, func(t *testing.T) {
			store := confirmationStore(t, codec, false)
			src := confirmationData()
			confirmationWrite(t, store, src)
			root := NewPage(bytes.Repeat([]byte{0x73}, 4096))
			defer root.Release()
			const offset = 37
			dst := root.Slice(offset, len(src))
			defer dst.Release()
			want := bytes.Clone(root.Data)
			copy(want[offset:], src)
			n, err := store.NewReader(1, len(src)).ReadAt(context.Background(), dst, 0)
			if err != nil || n != len(src) || !bytes.Equal(dst.Data, src) {
				t.Fatalf("read: n=%d err=%v outputMatches=%v", n, err, bytes.Equal(dst.Data, src))
			}
			for i := range want {
				if root.Data[i] != want[i] {
					t.Fatalf("cached-store read changed neighboring byte %d outside [%d,%d)", i, offset, offset+len(src))
				}
			}
		})
	}
}

// -race must remain clean for one ordinary partial-block write, including its
// background cache flush. Compression "none" and a real disk cache are key.
func TestCachedStoreUploadPublication(t *testing.T) {
	store := confirmationStore(t, "none", true)
	src := confirmationData()
	confirmationWrite(t, store, src)
	dst := NewPage(make([]byte, len(src)))
	defer dst.Release()
	n, err := store.NewReader(1, len(src)).ReadAt(context.Background(), dst, 0)
	if err != nil || n != len(src) || !bytes.Equal(dst.Data, src) {
		t.Fatalf("read: n=%d err=%v outputMatches=%v", n, err, bytes.Equal(dst.Data, src))
	}
}
