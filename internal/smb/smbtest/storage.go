package smbtest

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

// NewStorage builds a real JuiceFS adapter with SQLite metadata and a file
// object store in t.TempDir. Cleanup shuts down the adapter, filesystem and
// metadata in that order. Close any server fixtures before this cleanup runs.
// Setup and cleanup errors are reported through t.
func NewStorage(t testing.TB) *smbfs.FS {
	t.Helper()
	dir := t.TempDir()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mc := meta.DefaultConf()
	mc.NoBGJob = true
	mc.MaxDeletes = 0
	mc.Retries = 0
	database := filepath.Join(dir, "meta.db")
	metadata, err := meta.NewSQLite(database, mc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := metadata.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	format := meta.Format{Name: "smb-test", UUID: "smb-test", Storage: "file", BlockSize: 64, Compression: "none", DirStats: true}
	if initErr := metadata.Init(&format, true); initErr != nil {
		t.Fatal(initErr)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o700}
	if eno := metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	if sessionErr := metadata.NewSession(true); sessionErr != nil {
		t.Fatal(sessionErr)
	}
	cc := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: time.Second, PutTimeout: time.Second}
	store := chunk.NewCachedStore(blob, cc, nil)
	config := &vfs.Config{Meta: mc, Format: format, Chunk: &cc}
	native, err := jfs.NewFileSystem(config, metadata, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := native.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	barrier, err := smbfs.NewMetadataBarrier(database)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := smbfs.New(smbfs.Options{Filesystem: native, Barrier: barrier, MetadataPath: database, Config: config, Store: store})
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
