// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// Independent reduced reproducer for publication of writable pages to the
// asynchronous disk cache. One synchronous write, one handle, no test hooks.
func TestWriteBufferLifetimeDiskCachePublication(t *testing.T) {
	cc := chunk.Config{
		BlockSize: 4 << 20, MaxUpload: 1, MaxDownload: 1, BufferSize: 8 << 20,
		CacheDir: filepath.Join(t.TempDir(), "cache"), CacheSize: 4 << 20,
		CacheMode: 0600, CacheChecksum: chunk.CsExtend, CacheScanInterval: time.Hour,
		FreeSpace: 0.001, AutoCreate: true, Compress: "none", CacheFullBlock: true,
		MaxRetries: 1, GetTimeout: 3 * time.Second, PutTimeout: 3 * time.Second,
	}
	cc.SelfCheck("write-lifetime-test")
	disk, err := object.CreateStorage("file", filepath.Join(t.TempDir(), "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := fixtureWithChunkConfig(t, &failingStore{ObjectStorage: disk}, nil, &cc)
	h := openFile(t, f.s, "publication")
	want := lifetimeFixture(521, 0)
	if n, err := f.s.Write(h, want, 0, 0); err != nil || n != len(want) {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if err := f.s.Flush(h); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if n, err := f.s.Read(h, got, 0, 0); err != nil || n != len(got) {
		t.Fatalf("read n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("write stored different fixture bytes")
	}
}
