package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

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
