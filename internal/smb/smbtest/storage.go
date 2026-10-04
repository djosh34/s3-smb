package smbtest

import (
	"cmp"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

// NewStorage builds a real JuiceFS adapter with SQLite metadata and a file
// object store in t.TempDir. Cleanup shuts down the adapter, filesystem and
// metadata in that order. Close any server fixtures before this cleanup runs.
// Setup and cleanup errors are reported through t.
func NewStorage(t testing.TB) *smbfs.FS {
	t.Helper()
	dir := t.TempDir()
	return newStorage(t, dir, fileObjects(t, dir), S3Config{})
}

// S3Config sets the retry budget of NewS3Storage. Zero fields keep the values
// NewStorage uses: no metadata retries, one chunk retry, one second chunk
// timeouts and the adapter's default read retry window.
type S3Config struct {
	MetaRetries     int
	ChunkRetries    int
	Timeout         time.Duration
	ReadRetryWindow time.Duration
}

// NewS3Storage is NewStorage with the objects behind a local S3 endpoint and
// the returned fault proxy. JuiceFS talks to the proxy with its real S3 client,
// so proxy faults reach it as S3 delays, errors and outages.
func NewS3Storage(t testing.TB, config S3Config) (*smbfs.FS, *s3fault.Proxy) {
	t.Helper()
	dir := t.TempDir()
	files := fileObjects(t, dir)
	reads := http.StripPrefix("/bucket/", http.FileServer(http.Dir(filepath.Join(dir, "objects"))))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			reads.ServeHTTP(w, r)
			return
		}
		if err := files.Put(r.Context(), strings.TrimPrefix(r.URL.Path, "/bucket/"), r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(backend.Close)
	proxy, err := s3fault.New(t.Context(), backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	s3, err := object.CreateStorage("s3", proxy.URL()+"/bucket", "test-access", "test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	return newStorage(t, dir, s3, config), proxy
}

func fileObjects(t testing.TB, dir string) object.ObjectStorage {
	t.Helper()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func newStorage(t testing.TB, dir string, blob object.ObjectStorage, config S3Config) *smbfs.FS {
	t.Helper()
	mc := meta.DefaultConf()
	mc.NoBGJob = true
	mc.MaxDeletes = 0
	mc.Retries = config.MetaRetries
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
	timeout := cmp.Or(config.Timeout, time.Second)
	cc := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: cmp.Or(config.ChunkRetries, 1), GetTimeout: timeout, PutTimeout: timeout}
	store := chunk.NewCachedStore(blob, cc, nil)
	vfsConfig := &vfs.Config{Meta: mc, Format: format, Chunk: &cc}
	native, err := jfs.NewFileSystem(vfsConfig, metadata, store, nil)
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
	adapter, err := smbfs.New(smbfs.Options{Filesystem: native, Barrier: barrier, MetadataPath: database, Config: vfsConfig, Store: store, ReadRetryWindow: config.ReadRetryWindow})
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
