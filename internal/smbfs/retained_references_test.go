package smbfs

import (
	"strconv"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestRetainedReferencesWorkWithTrash(t *testing.T) {
	for _, days := range []int{0, 14} {
		t.Run(strconv.Itoa(days), func(t *testing.T) { testRetainedReferences(t, days) })
	}
}

func testRetainedReferences(t *testing.T, days int) {
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
	if days == 0 {
		assertDiscardedInode(t, f, r.Object.Inode)
	}
}

func assertDiscardedInode(t *testing.T, f *fixture, ino smb.Inode) {
	t.Helper()
	for _, query := range []string{"SELECT count(*) FROM jfs_node WHERE inode=?", "SELECT count(*) FROM jfs_xattr WHERE inode=?"} {
		var count int
		if err := f.fs.directory.QueryRowContext(t.Context(), query, ino).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("last close left %d rows: %s", count, query)
		}
	}
}
