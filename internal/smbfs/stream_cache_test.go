package smbfs

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

func TestLastCloseClearsStreamCacheWhileStateIsPinned(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	bh := f.open(t, base.Object, smb.AccessRead)
	sh := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, sh, "old", 0)
	st, unpin := f.fs.pin(base.Object.Inode)
	defer unpin()
	if err := f.fs.Close(t.Context(), sh); err != nil {
		t.Fatal(err)
	}
	if data, cached := st.cachedStream("fork"); !cached || string(data) != "old" {
		t.Fatalf("cache cleared before last reference: %q, %v", data, cached)
	}
	if err := f.fs.Close(t.Context(), bh); err != nil {
		t.Fatal(err)
	}
	// Keep the old state pinned like an attribute query, then change the stored
	// stream while no reference is open. Reopening the base retains that state.
	if eno := f.metadata.SetXattr(storageContext(t.Context()), meta.Ino(base.Object.Inode), "fork", []byte("new data"), meta.XattrReplace); eno != 0 {
		t.Fatal(eno)
	}
	f.open(t, base.Object, smb.AccessRead)
	reopened := f.open(t, stream.Object, smb.AccessRead)
	read(t, f.fs, reopened, []byte("new data"))
}
