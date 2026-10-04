package smbfs

import (
	"io"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestBackendFileSizeBoundary(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "abc", 0)
	puts := f.store.puts.Load()
	for _, size := range []uint64{maxFileSize, maxFileSize + 1, math.MaxUint64} {
		requireError(t, f.fs.Truncate(t.Context(), h, size), smb.ErrFileTooLarge)
		requireError(t, f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}), smb.ErrFileTooLarge)
		_, err := f.fs.WriteAt(t.Context(), h, []byte("x"), size-1)
		requireError(t, err, smb.ErrFileTooLarge)
		_, err = f.fs.ReadAt(t.Context(), h, make([]byte, 1), size-1)
		requireError(t, err, io.EOF)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != 3 || f.store.puts.Load() != puts {
		t.Fatalf("invalid range mutated data: %+v, %v, puts=%d", a, err, f.store.puts.Load())
	}
	read(t, f.fs, h, []byte("abc"))
	if err = f.fs.Truncate(t.Context(), h, maxFileSize-1); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, h, "z", maxFileSize-2)
	data := make([]byte, 1)
	n, err := f.fs.ReadAt(t.Context(), h, data, maxFileSize-2)
	if err != nil || n != 1 || data[0] != 'z' {
		t.Fatalf("last supported byte = %q, %d, %v", data, n, err)
	}
	if err = f.fs.Truncate(t.Context(), h, 3); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte("abc"))
}

func TestReadAcrossBackendBoundaryClipsAtEOF(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	if err := f.fs.Truncate(t.Context(), h, maxFileSize-1); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, h, "z", maxFileSize-2)
	data := make([]byte, 2)
	n, err := f.fs.ReadAt(t.Context(), h, data, maxFileSize-2)
	if n != 1 || data[0] != 'z' {
		t.Fatalf("read across boundary = %q, %d, %v", data, n, err)
	}
	requireError(t, err, io.EOF)
}

func TestReadPastEOFAtLargeOffsets(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	for _, key := range []smb.ObjectKey{r.Object, stream.Object} {
		h := f.open(t, key, smb.AccessRead|smb.AccessWrite)
		write(t, f.fs, h, "data", 0)
		for _, offset := range []uint64{4, maxFileSize - 1, maxFileSize, math.MaxUint64} {
			n, err := f.fs.ReadAt(t.Context(), h, make([]byte, 1), offset)
			if n != 0 || smb.StatusFromError(err) != smb.StatusEndOfFile {
				t.Fatalf("offset %d: read=%d, %v, status=%x", offset, n, err, smb.StatusFromError(err))
			}
		}
	}
}
