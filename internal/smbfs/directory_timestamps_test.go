package smbfs

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestDirectoryExplicitTimesAfterBufferedWrite(t *testing.T) {
	for _, stamp := range []time.Time{time.Unix(1000000000, 123456700).UTC(), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)} {
		t.Run(stamp.Format(time.RFC3339Nano), func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			h := f.open(t, r.Object, smb.AccessWrite)
			write(t, f.fs, h, "abcdef", 0)
			if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp, Changed: &stamp}); err != nil {
				t.Fatal(err)
			}
			page, err := f.fs.ReadDir(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 {
				t.Fatalf("page after explicit times = %+v, %v", page, err)
			}
			if !page[0].Attr.Modified.Equal(stamp) || !page[0].Attr.Changed.Equal(stamp) {
				t.Fatalf("page lost explicit times: %+v", page[0].Attr)
			}
		})
	}
}

func TestDirectoryCommittedWriteTimesAvoidPerEntryQueries(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	want, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedMetadata{Meta: f.metadata}
	f.fs.metadata = counted
	page, err := f.fs.ReadDir(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("committed page = %+v, %v", page, err)
	}
	if !page[0].Attr.Modified.Equal(want.Modified) {
		t.Fatalf("committed modified time = %v, want %v", page[0].Attr.Modified, want.Modified)
	}
	if counted.attrs.Load() != 1 || counted.xattrs.Load() != 0 {
		t.Fatalf("committed entry was reread: attrs=%d, xattrs=%d", counted.attrs.Load(), counted.xattrs.Load())
	}
}

func TestDirectoryModifiedTimeAcrossFlushCompletion(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	old := time.Unix(1000000000, 0).UTC()
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &old}); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, h, "abcdef", 0)
	generation := f.fs.commits.Load()
	page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("pre-flush page = %+v, %v", page, err)
	}
	if err = f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	want, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil {
		t.Fatal(err)
	}
	if !want.Modified.After(old) {
		t.Fatalf("flush did not commit the write time: %v", want.Modified)
	}
	got, err := f.fs.directoryAttr(t.Context(), page[0], generation)
	if err != nil || !got.Modified.Equal(want.Modified) || got.Size != want.Size {
		t.Fatalf("page across flush = %+v, %v; want %+v", got, err, want)
	}
}
