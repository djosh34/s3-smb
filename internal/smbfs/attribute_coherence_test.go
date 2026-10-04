package smbfs

import (
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

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
