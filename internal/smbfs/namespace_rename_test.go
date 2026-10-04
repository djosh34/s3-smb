package smbfs

import (
	"errors"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestDirectoryRenameRejectsDescendant(t *testing.T) {
	f := newFixture(t, 0)
	source := f.create(t, "a", smb.KindDirectory)
	f.create(t, "a/b", smb.KindDirectory)
	descendant := f.create(t, "a/b/c", smb.KindDirectory)
	err := f.fs.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: smb.Name{Parent: descendant.Object.Inode, Base: "moved"}, SourceInode: source.Object.Inode})
	if !errors.Is(err, smb.ErrInvalidParameter) {
		t.Fatalf("descendant move = %v", err)
	}
	r, err := f.fs.Lookup(t.Context(), "a/b/c")
	if err != nil || !r.Exists {
		t.Fatalf("rejected move changed subtree: %+v, %v", r, err)
	}
	destination := f.create(t, "destination", smb.KindDirectory)
	if err = f.fs.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: smb.Name{Parent: destination.Object.Inode, Base: "moved"}, SourceInode: source.Object.Inode}); err != nil {
		t.Fatal(err)
	}
	r, err = f.fs.Lookup(t.Context(), "destination/moved/b/c")
	if err != nil || !r.Exists || r.Object.Inode != descendant.Object.Inode {
		t.Fatalf("ordinary move lost subtree: %+v, %v", r, err)
	}
}
