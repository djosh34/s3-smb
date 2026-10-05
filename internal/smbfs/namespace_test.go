package smbfs

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestLookupPaths(t *testing.T) {
	f := newFixture(t, 0)
	root, err := f.fs.Lookup(t.Context(), "")
	if err != nil || !root.Exists || root.Object.Inode != 1 || root.Attr.Kind != smb.KindDirectory {
		t.Fatalf("root = %+v, %v", root, err)
	}
	dir := f.create(t, "dir", smb.KindDirectory)
	f.create(t, "dir/a", smb.KindFile)
	nested, err := f.fs.Lookup(t.Context(), "dir\\a::$DATA")
	if err != nil || !nested.Exists || nested.Object.Stream != "" {
		t.Fatalf("nested = %+v, %v", nested, err)
	}
	for _, p := range []string{"../data", "dir/../data", "/data", "dir//a", "dir/.", ".trash/x", ".stats", "dir/a\x00", "dir/a:bad:other", "dir/a:", "dir/a:" + strings.Repeat("x", 256)} {
		if _, err = f.fs.Lookup(t.Context(), p); err == nil {
			t.Errorf("accepted path %q", p)
		}
	}
	_, err = f.fs.Lookup(t.Context(), "absent/leaf")
	requireError(t, err, smb.ErrPathNotFound)
	missing, err := f.fs.Lookup(t.Context(), "dir/missing")
	if err != nil || missing.Exists || missing.Name.Parent != dir.Object.Inode {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
	_, err = f.fs.Open(t.Context(), smb.ObjectKey{}, smb.AccessRead)
	requireError(t, err, smb.ErrInvalidParameter)
}

func TestCreateIsExclusive(t *testing.T) {
	f := newFixture(t, 0)
	f.create(t, "base", smb.KindFile)
	for _, p := range []string{"data", "base:fork"} {
		missing, err := f.fs.Lookup(t.Context(), p)
		if err != nil || missing.Exists {
			t.Fatalf("%s: missing lookup = %+v, %v", p, missing, err)
		}
		created, err := f.fs.Create(t.Context(), missing.Name, smb.KindFile)
		if err != nil {
			t.Fatal(err)
		}
		h := f.open(t, created.Object, smb.AccessRead|smb.AccessWrite)
		read(t, f.fs, h, []byte{})
		write(t, f.fs, h, "created", 0)
		_, err = f.fs.Create(t.Context(), missing.Name, smb.KindFile)
		requireError(t, err, smb.ErrNameCollision)
		read(t, f.fs, h, []byte("created"))
	}
	_, err := f.fs.Create(t.Context(), smb.Name{Parent: 1, Base: "absent", Stream: "fork"}, smb.KindFile)
	requireError(t, err, smb.ErrNameNotFound)
}

func TestNamespaceChangesRequireExpectedIdentities(t *testing.T) {
	for _, trashDays := range []int{0, 14} {
		t.Run(strconv.Itoa(trashDays), func(t *testing.T) {
			f := fixtureAt(t, t.TempDir(), 0, true, trashDays)
			source := f.create(t, "source", smb.KindFile)
			destination := f.create(t, "destination", smb.KindFile)
			a := f.open(t, source.Object, smb.AccessRead|smb.AccessWrite)
			b := f.open(t, destination.Object, smb.AccessRead|smb.AccessWrite)
			write(t, f.fs, a, "source", 0)
			write(t, f.fs, b, "destination", 0)
			requireError(t, f.fs.Remove(t.Context(), source.Name, destination.Object.Inode), smb.ErrIdentityChanged)
			request := smb.RenameRequest{Source: source.Name, Destination: destination.Name, SourceInode: source.Object.Inode, Replace: true}
			requireError(t, f.fs.Rename(t.Context(), request), smb.ErrIdentityChanged)
			request.DestinationInode = destination.Object.Inode
			request.SourceInode = destination.Object.Inode
			requireError(t, f.fs.Rename(t.Context(), request), smb.ErrIdentityChanged)
			read(t, f.fs, a, []byte("source"))
			read(t, f.fs, b, []byte("destination"))
			request.SourceInode = source.Object.Inode
			if err := f.fs.Rename(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			path, err := f.fs.PathOf(t.Context(), source.Object.Inode)
			if err != nil || path != "destination" {
				t.Fatalf("path = %q, %v", path, err)
			}
			_, err = f.fs.PathOf(t.Context(), destination.Object.Inode)
			requireError(t, err, smb.ErrNameNotFound)
			read(t, f.fs, a, []byte("source"))
			read(t, f.fs, b, []byte("destination"))
			if err = f.fs.Remove(t.Context(), destination.Name, source.Object.Inode); err != nil {
				t.Fatal(err)
			}
			_, err = f.fs.PathOf(t.Context(), source.Object.Inode)
			requireError(t, err, smb.ErrNameNotFound)
			read(t, f.fs, a, []byte("source"))
		})
	}
}

func TestNamedStreamRenameIsNotSupported(t *testing.T) {
	f := newFixture(t, 0)
	f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	h := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "stream", 0)
	destination := stream.Name
	destination.Stream = "other"
	requireError(t, f.fs.Rename(t.Context(), smb.RenameRequest{Source: stream.Name, Destination: destination, SourceInode: stream.Object.Inode}), smb.ErrNotSupported)
	read(t, f.fs, h, []byte("stream"))
	missing, err := f.fs.Lookup(t.Context(), "data:other")
	if err != nil || missing.Exists {
		t.Fatalf("rename created destination: %+v, %v", missing, err)
	}
	destination.Stream = ""
	requireError(t, f.fs.Rename(t.Context(), smb.RenameRequest{Source: destination, Destination: stream.Name, SourceInode: stream.Object.Inode}), smb.ErrNotSupported)
}

func TestDirectoryRenameRejectsDescendant(t *testing.T) {
	f := newFixture(t, 0)
	source := f.create(t, "a", smb.KindDirectory)
	f.create(t, "a/b", smb.KindDirectory)
	descendant := f.create(t, "a/b/c", smb.KindDirectory)
	err := f.fs.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: smb.Name{Parent: descendant.Object.Inode, Base: "moved"}, SourceInode: source.Object.Inode})
	requireError(t, err, smb.ErrInvalidParameter)
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

func TestReadOnlyRejectsAllMutations(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	ro := f.readOnly(t)
	for _, r := range []smb.Resolved{base, stream} {
		for _, access := range []smb.Access{smb.AccessWrite, smb.AccessAppend, smb.AccessWrite | smb.AccessAppend} {
			_, err := ro.Open(t.Context(), r.Object, access)
			requireError(t, err, smb.ErrReadOnly)
		}
		h, err := ro.Open(t.Context(), r.Object, smb.AccessRead)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ro.WriteAt(t.Context(), h, []byte("x"), 0)
		requireError(t, err, smb.ErrReadOnly)
		requireError(t, ro.Truncate(t.Context(), h, 0), smb.ErrReadOnly)
		size := uint64(0)
		requireError(t, ro.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}), smb.ErrReadOnly)
		requireError(t, ro.Remove(t.Context(), r.Name, r.Object.Inode), smb.ErrReadOnly)
		if err = ro.Close(t.Context(), h); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ro.Create(t.Context(), smb.Name{Parent: 1, Base: "new"}, smb.KindFile)
	requireError(t, err, smb.ErrReadOnly)
	requireError(t, ro.Rename(t.Context(), smb.RenameRequest{Source: base.Name, Destination: smb.Name{Parent: 1, Base: "new"}, SourceInode: base.Object.Inode}), smb.ErrReadOnly)
}

// An open file stays usable after its name is removed, with or without trash,
// and its rows are gone after the last close.
func TestRetainedReferencesWorkWithTrash(t *testing.T) {
	for _, days := range []int{0, 14} {
		t.Run(strconv.Itoa(days), func(t *testing.T) { checkRetainedReferences(t, days) })
	}
}

func checkRetainedReferences(t *testing.T, days int) {
	t.Helper()
	f := fixtureAt(t, t.TempDir(), 0, true, days)
	r := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	sh := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "base", 0)
	write(t, f.fs, sh, "stream", 0)
	if err := f.fs.Remove(t.Context(), r.Name, r.Object.Inode); err != nil {
		t.Fatal(err)
	}
	_, err := f.fs.PathOf(t.Context(), r.Object.Inode)
	requireError(t, err, smb.ErrNameNotFound)
	read(t, f.fs, h, []byte("base"))
	read(t, f.fs, sh, []byte("stream"))
	write(t, f.fs, h, "new", 0)
	write(t, f.fs, sh, "new", 0)
	if err = f.fs.Truncate(t.Context(), h, 3); err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Truncate(t.Context(), sh, 3); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1000000000, 123456700)
	bits := uint32(0x21)
	if err = f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp, Created: &stamp, Attributes: &bits}); err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte("new"))
	read(t, f.fs, sh, []byte("new"))
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Modified.Equal(stamp) {
		t.Fatalf("retained attrs = %+v, %v", a, err)
	}
	if _, err = f.fs.Lookup(t.Context(), ".trash"); err == nil {
		t.Fatal("trash admitted into namespace")
	}
	if err = f.fs.Close(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Close(t.Context(), sh); err != nil {
		t.Fatal(err)
	}
	if days != 0 {
		return
	}
	for _, query := range []string{"SELECT count(*) FROM jfs_node WHERE inode=?", "SELECT count(*) FROM jfs_xattr WHERE inode=?"} {
		var count int
		if err = f.fs.directory.QueryRowContext(t.Context(), query, r.Object.Inode).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("last close left %d rows: %s", count, query)
		}
	}
}
