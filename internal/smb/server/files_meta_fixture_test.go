package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

// newFilesMetaStorage uses file-backed JuiceFS data and SQLite metadata.
// Register server cleanup after this helper so opens close before storage.
func newFilesMetaStorage(t *testing.T) *smbfs.FS {
	t.Helper()
	dir := t.TempDir()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	conf.Retries = 0
	database := filepath.Join(dir, "meta.db")
	metadata, err := meta.NewSQLite(database, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := metadata.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	format := meta.Format{Name: "files-meta-test", UUID: "files-meta-fixture", Storage: "file", BlockSize: 64, Compression: "none", DirStats: true}
	if err = metadata.Init(&format, true); err != nil {
		t.Fatal(err)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o700}
	if errno := metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); errno != 0 {
		t.Fatal(errno)
	}
	if err = metadata.NewSession(true); err != nil {
		t.Fatal(err)
	}
	chunks := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: 5 * time.Second, PutTimeout: time.Second}
	store := chunk.NewCachedStore(blob, chunks, nil)
	config := &vfs.Config{Meta: conf, Format: format, Chunk: &chunks}
	filesystem, err := jfs.NewFileSystem(config, metadata, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := filesystem.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	barrier, err := smbfs.NewMetadataBarrier(database)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := smbfs.New(smbfs.Options{Filesystem: filesystem, Barrier: barrier, MetadataPath: database, Config: config, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return storage
}

func TestFilesMetaStorageRoot(t *testing.T) {
	storage := newFilesMetaStorage(t)
	root, err := storage.Lookup(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !root.Exists || root.Attr.Kind != smb.KindDirectory || root.Object.Inode == 0 {
		t.Fatalf("root = %+v", root)
	}
}
