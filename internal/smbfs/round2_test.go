package smbfs

import (
	"io"
	"math"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

type afterAttributeRead struct {
	meta.Meta
	after func()
	inode meta.Ino
	armed atomic.Bool
}

func (m *afterAttributeRead) GetAttr(ctx meta.Context, ino meta.Ino, a *meta.Attr) syscall.Errno {
	eno := m.Meta.GetAttr(ctx, ino, a)
	if eno == 0 && ino == m.inode && m.armed.CompareAndSwap(true, false) {
		m.after()
	}
	return eno
}

func TestAttributeLengthAcrossFlushCompletion(t *testing.T) {
	for _, lookup := range []bool{false, true} {
		t.Run(map[bool]string{false: "getattr", true: "lookup"}[lookup], func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			h := f.open(t, r.Object, smb.AccessWrite)
			write(t, f.fs, h, "abcdef", 0)
			wrapped := &afterAttributeRead{Meta: f.metadata, inode: meta.Ino(r.Object.Inode), after: func() {
				if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
					t.Error(err)
				}
			}}
			wrapped.armed.Store(true)
			f.fs.metadata = wrapped
			var attr smb.Attr
			var err error
			if lookup {
				var resolved smb.Resolved
				resolved, err = f.fs.Lookup(t.Context(), "data")
				attr = resolved.Attr
			} else {
				attr, err = f.fs.GetAttr(t.Context(), r.Object)
			}
			if err != nil || attr.Size != 6 {
				t.Fatalf("query across flush = %+v, %v", attr, err)
			}
			if wrapped.armed.Load() {
				t.Fatal("flush race was not exercised")
			}
		})
	}
}

func TestDirectoryLengthAcrossFlushAndClose(t *testing.T) {
	for _, closeRef := range []bool{false, true} {
		t.Run(map[bool]string{false: "flush", true: "close"}[closeRef], func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			h := f.open(t, r.Object, smb.AccessWrite)
			write(t, f.fs, h, "abcdef", 0)
			generation := f.fs.commits.Load()
			page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 || page[0].attr.Length != 0 {
				t.Fatalf("pre-commit SQL snapshot = %+v, %v", page, err)
			}
			if closeRef {
				err = f.fs.Close(t.Context(), h)
			} else {
				err = f.fs.Flush(t.Context(), h, smb.SyncData)
			}
			if err != nil {
				t.Fatal(err)
			}
			attr, err := f.fs.directoryAttr(t.Context(), page[0], generation)
			if err != nil || attr.Size != 6 {
				t.Fatalf("page across commit = %+v, %v", attr, err)
			}
		})
	}
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
