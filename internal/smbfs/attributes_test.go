package smbfs

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

func TestAttributesPersistAndStayOutOfStreams(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	stamp := time.Unix(-123456, 123456789).UTC()
	attributes := uint32(0x22)
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Created: &stamp, Accessed: &stamp, Modified: &stamp, Changed: &stamp, Attributes: &attributes}); err != nil {
		t.Fatal(err)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Created.Equal(stamp) || !a.Accessed.Equal(stamp) || !a.Modified.Equal(stamp) || !a.Changed.Equal(stamp) || a.Attributes != attributes {
		t.Fatalf("attr = %+v, %v", a, err)
	}
	streams, err := f.fs.Streams(t.Context(), r.Object.Inode)
	if err != nil || len(streams) != 0 {
		t.Fatalf("private attributes listed as streams: %+v, %v", streams, err)
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

func TestIssue96ExplicitTimestampSurvivesFlush(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	writer := f.open(t, r.Object, smb.AccessWrite)
	other := f.open(t, r.Object, smb.AccessRead)
	write(t, f.fs, writer, "dirty", 0)
	stamp := time.Unix(1000000000, 123456700).UTC()
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Flush(t.Context(), other, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Modified.Equal(stamp) {
		t.Fatalf("mtime = %v, %v; want %v", a.Modified, err, stamp)
	}
}

func TestLaterWritesUpdateExplicitTimesButKeepCreationTime(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	initial := r.Attr.Created
	h := f.open(t, r.Object, smb.AccessWrite)
	stamp := time.Unix(1000000000, 123456700)
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp, Changed: &stamp}); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, h, "data", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Modified.After(stamp) || !a.Changed.After(stamp) || !a.Created.Equal(initial) {
		t.Fatalf("later attrs = %+v, %v", a, err)
	}
}

func TestFlushTimesStayUnchangedAfterLastClose(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	open, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Close(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	closed, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !open.Modified.Equal(closed.Modified) || !open.Changed.Equal(closed.Changed) {
		t.Fatalf("times changed on close: open=%+v, closed=%+v, %v", open, closed, err)
	}
}

func TestIssue123SelectedStreamUsesFileCalls(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	baseHandle := f.open(t, base.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, baseHandle, "base bytes", 0)
	stream := f.create(t, "data:com.apple.ResourceFork:$DATA", smb.KindFile)
	other := f.create(t, "data:AFP_AfpInfo", smb.KindFile)
	h := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	otherHandle := f.open(t, other.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, otherHandle, "finder", 0)
	write(t, f.fs, h, "abc", 4)
	write(t, f.fs, h, "Z", 5)
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a', 'Z', 'c'})
	size := uint64(5)
	if err := f.fs.SetAttr(t.Context(), h.Key(), smb.AttrChange{Size: &size}); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a'})
	if err := f.fs.Truncate(t.Context(), h, 8); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte{0, 0, 0, 0, 'a', 0, 0, 0})
	write(t, f.fs, h, "!", maxStreamSize-1)
	_, err := f.fs.WriteAt(t.Context(), h, []byte("!"), maxStreamSize)
	requireError(t, err, smb.ErrFileTooLarge)
	requireError(t, f.fs.Truncate(t.Context(), h, maxStreamSize+1), smb.ErrFileTooLarge)
	attr, err := f.fs.GetAttr(t.Context(), h.Key())
	if err != nil || attr.Size != maxStreamSize {
		t.Fatalf("stream attr = %+v, %v", attr, err)
	}
	streams, err := f.fs.Streams(t.Context(), base.Object.Inode)
	if err != nil || len(streams) != 2 {
		t.Fatalf("streams = %+v, %v", streams, err)
	}
	if err = f.fs.Remove(t.Context(), stream.Name, base.Object.Inode); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, baseHandle, []byte("base bytes"))
	read(t, f.fs, otherHandle, []byte("finder"))
	_, err = f.fs.GetAttr(t.Context(), h.Key())
	requireError(t, err, smb.ErrNameNotFound)
}

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

func TestSizeCap(t *testing.T) {
	for _, path := range []string{"data", "data:fork"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t, 0)
			base := f.create(t, "data", smb.KindFile)
			selected := base
			if path != "data" {
				selected = f.create(t, path, smb.KindFile)
			}
			h := f.open(t, selected.Object, smb.AccessRead|smb.AccessWrite)
			write(t, f.fs, h, "retained", 0)
			before, err := f.fs.GetAttr(t.Context(), selected.Object)
			if err != nil {
				t.Fatal(err)
			}
			// A cap at or above EOF changes neither length nor times.
			for _, limit := range []uint64{8, 8192} {
				if err = f.fs.SetAttr(t.Context(), selected.Object, smb.AttrChange{SizeCap: &limit}); err != nil {
					t.Fatal(err)
				}
				after, attrErr := f.fs.GetAttr(t.Context(), selected.Object)
				if attrErr != nil || after != before {
					t.Fatalf("cap %d changed attributes: %+v, want %+v; %v", limit, after, before, attrErr)
				}
			}
			limit, oversized := uint64(3), uint64(1<<63)
			requireError(t, f.fs.SetAttr(t.Context(), selected.Object, smb.AttrChange{Size: &limit, SizeCap: &limit}), smb.ErrInvalidParameter)
			requireError(t, f.fs.SetAttr(t.Context(), selected.Object, smb.AttrChange{SizeCap: &oversized}), smb.ErrFileTooLarge)
			if err = f.fs.SetAttr(t.Context(), selected.Object, smb.AttrChange{SizeCap: &limit}); err != nil {
				t.Fatal(err)
			}
			if err = f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
				t.Fatal(err)
			}
			read(t, f.fs, h, []byte("ret"))
			if attr, attrErr := f.fs.GetAttr(t.Context(), base.Object); path != "data" && (attrErr != nil || attr.Size != 0) {
				t.Fatalf("stream cap changed base length: %+v, %v", attr, attrErr)
			}
		})
	}
	f := newFixture(t, 0)
	directory := f.create(t, "dir", smb.KindDirectory)
	limit := uint64(0)
	requireError(t, f.fs.SetAttr(t.Context(), directory.Object, smb.AttrChange{SizeCap: &limit}), smb.ErrIsDirectory)
}

func TestStatFSConfiguredCapacityAndDefaultCap(t *testing.T) {
	for _, capacity := range []uint64{0, 500000000000, 2000000000000} {
		f := newFixture(t, capacity)
		space, err := f.fs.StatFS(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if space.Available > space.Free || space.Free > space.Capacity || space.VolumeID == 0 {
			t.Fatalf("space = %+v", space)
		}
		if capacity != 0 && space.Capacity != capacity {
			t.Fatalf("capacity = %d; want %d", space.Capacity, capacity)
		}
		if capacity == 0 && space.Free > 1<<40 {
			t.Fatalf("unlimited free = %d", space.Free)
		}
		if capacity > 1<<40 && space.Free <= 1<<40 {
			t.Fatal("configured capacity was capped")
		}
	}
}
