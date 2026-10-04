package smbfs

import (
	"testing"

	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
)

func TestConstructorUsesCurrentFormat(t *testing.T) {
	f := newFixture(t, 0)
	counted := &countedMetadata{Meta: f.metadata}
	filesystem, err := jfs.NewFileSystem(f.config, counted, f.chunks, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted.denyLoad = true
	adapter, err := New(Options{Filesystem: filesystem, Barrier: f.fs.barrier, Config: f.config, Store: f.chunks, MetadataPath: f.path})
	if err != nil {
		t.Error(err)
	} else if err = adapter.Shutdown(); err != nil {
		t.Error(err)
	}
	if err = filesystem.Close(); err != nil {
		t.Fatal(err)
	}
}
