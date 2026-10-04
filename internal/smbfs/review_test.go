package smbfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCloseCanceledContextStillReleases(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "data", 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireError(t, f.fs.Close(ctx, h), context.Canceled)
	requireError(t, f.fs.Close(t.Context(), h), smb.ErrInvalidHandle)
	if len(f.fs.inodes) != 0 {
		t.Fatal("canceled close retained native reference")
	}
}

func TestDirectoryCookieSurvivesRemoval(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	a := f.create(t, "dir/a", smb.KindFile)
	f.create(t, "dir/b", smb.KindFile)
	f.create(t, "dir/c", smb.KindFile)
	f.create(t, "dir/d", smb.KindFile)
	page, err := f.fs.ReadDir(t.Context(), dir.Object.Inode, 0, 2)
	if err != nil || len(page) != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	cookie := page[1].Next
	if err = f.fs.Remove(t.Context(), a.Name, a.Object.Inode); err != nil {
		t.Fatal(err)
	}
	page, err = f.fs.ReadDir(t.Context(), dir.Object.Inode, cookie, 2)
	if err != nil || len(page) != 2 || page[0].Name != "c" || page[1].Name != "d" {
		t.Fatalf("continuation skipped entries: %+v, %v", page, err)
	}
}

func TestNonDataAttributeChangesDoNotUpload(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "dirty", 0)
	puts := f.store.puts.Load()
	stamp := time.Unix(1000000000, 0)
	bits := uint32(2)
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Created: &stamp, Attributes: &bits}); err != nil {
		t.Fatal(err)
	}
	if f.store.puts.Load() != puts {
		t.Fatal("non-data attribute change uploaded buffered data")
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != 5 || !a.Created.Equal(stamp) || a.Attributes != bits {
		t.Fatalf("attr = %+v, %v", a, err)
	}
}

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
}
