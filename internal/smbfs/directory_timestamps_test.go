package smbfs

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
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
	for _, initial := range []time.Time{time.Unix(1000000000, 0).UTC(), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)} {
		t.Run(initial.Format(time.RFC3339Nano), func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			h := f.open(t, r.Object, smb.AccessWrite)
			if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &initial, Changed: &initial}); err != nil {
				t.Fatal(err)
			}
			write(t, f.fs, h, "abcdef", 0)
			generation := f.fs.directoryGeneration()
			page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 {
				t.Fatalf("pre-flush page = %+v, %v", page, err)
			}
			if err = f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
				t.Fatal(err)
			}
			var raw meta.Attr
			if eno := f.metadata.GetAttr(storageContext(t.Context()), meta.Ino(r.Object.Inode), &raw); eno != 0 {
				t.Fatal(eno)
			}
			modified := time.Unix(raw.Mtime, int64(raw.Mtimensec)).UTC()
			changed := time.Unix(raw.Ctime, int64(raw.Ctimensec)).UTC()
			if modified.Equal(initial) || changed.Equal(initial) {
				t.Fatal("flush did not replace the explicit times")
			}
			got, err := f.fs.directoryAttr(t.Context(), page[0], generation)
			if err != nil || !got.Modified.Equal(modified) || !got.Changed.Equal(changed) || got.Size != raw.Length {
				t.Fatalf("page across flush = %+v, %v; want modified=%v, changed=%v, size=%d", got, err, modified, changed, raw.Length)
			}
		})
	}
}

func TestDirectoryTimesAcrossTruncateAndReopen(t *testing.T) {
	for _, operation := range []string{"truncate_buffered", "set_attr_size", "truncate_clean", "close_and_reopen"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			h := f.open(t, r.Object, smb.AccessWrite)
			initial := time.Unix(1000000000, 0).UTC()
			if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &initial, Changed: &initial}); err != nil {
				t.Fatal(err)
			}
			if operation != "truncate_clean" {
				write(t, f.fs, h, "abcdef", 0)
			}
			generation := f.fs.directoryGeneration()
			page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 {
				t.Fatalf("pre-mutation page = %+v, %v", page, err)
			}
			switch operation {
			case "truncate_buffered", "truncate_clean":
				err = f.fs.Truncate(t.Context(), h, 8)
			case "set_attr_size":
				size := uint64(8)
				err = f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size})
			case "close_and_reopen":
				err = f.fs.Close(t.Context(), h)
				if err == nil {
					f.open(t, r.Object, smb.AccessRead)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := f.fs.GetAttr(t.Context(), r.Object)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.fs.directoryAttr(t.Context(), page[0], generation)
			if err != nil || !got.Modified.Equal(want.Modified) || !got.Changed.Equal(want.Changed) || got.Size != want.Size {
				t.Fatalf("page across %s = %+v, %v; want %+v", operation, got, err, want)
			}
		})
	}
}

func TestDirectoryTimesAcrossSetAttr(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_handle", true: "with_handle"}[retained], func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			if retained {
				f.open(t, r.Object, smb.AccessRead)
			}
			initial := time.Unix(1000000000, 0).UTC()
			if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Accessed: &initial, Modified: &initial, Changed: &initial}); err != nil {
				t.Fatal(err)
			}
			generation := f.fs.directoryGeneration()
			page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 {
				t.Fatalf("pre-setattr page = %+v, %v", page, err)
			}
			stamp := time.Date(2100, 1, 1, 0, 0, 0, 123456700, time.UTC)
			if err = f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Accessed: &stamp, Modified: &stamp, Changed: &stamp}); err != nil {
				t.Fatal(err)
			}
			got, err := f.fs.directoryAttr(t.Context(), page[0], generation)
			if err != nil || !got.Accessed.Equal(stamp) || !got.Modified.Equal(stamp) || !got.Changed.Equal(stamp) {
				t.Fatalf("page across setattr = %+v, %v; want times %v", got, err, stamp)
			}
		})
	}
}

func TestDirectoryTimesAcrossFailedSetAttr(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	f.open(t, r.Object, smb.AccessRead)
	generation := f.fs.directoryGeneration()
	page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("pre-setattr page = %+v, %v", page, err)
	}
	stamp := time.Unix(1000000000, 123456700).UTC()
	invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	// Accessed is stored before Modified fails to marshal as an exact time.
	err = f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Accessed: &stamp, Modified: &invalid})
	requireError(t, err, smb.ErrInvalidParameter)
	got, err := f.fs.directoryAttr(t.Context(), page[0], generation)
	if err != nil || !got.Accessed.Equal(stamp) {
		t.Fatalf("page across failed setattr = %+v, %v; want accessed %v", got, err, stamp)
	}
}

func TestDirectoryTimesAcrossStreamChanges(t *testing.T) {
	for _, operation := range []string{"write", "truncate", "set_attr_size"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			stream := f.create(t, "data:resource", smb.KindFile)
			h := f.open(t, stream.Object, smb.AccessWrite)
			initial := time.Unix(1000000000, 0).UTC()
			if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &initial, Changed: &initial}); err != nil {
				t.Fatal(err)
			}
			generation := f.fs.directoryGeneration()
			page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 {
				t.Fatalf("pre-stream-change page = %+v, %v", page, err)
			}
			switch operation {
			case "write":
				write(t, f.fs, h, "stream data", 0)
			case "truncate":
				err = f.fs.Truncate(t.Context(), h, 8)
			case "set_attr_size":
				size := uint64(8)
				err = f.fs.SetAttr(t.Context(), stream.Object, smb.AttrChange{Size: &size})
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := f.fs.GetAttr(t.Context(), r.Object)
			if err != nil {
				t.Fatal(err)
			}
			if want.Modified.Equal(initial) || want.Changed.Equal(initial) {
				t.Fatal("stream change did not replace the explicit times")
			}
			got, err := f.fs.directoryAttr(t.Context(), page[0], generation)
			if err != nil || !got.Modified.Equal(want.Modified) || !got.Changed.Equal(want.Changed) || got.Size != want.Size {
				t.Fatalf("page across stream %s = %+v, %v; want %+v", operation, got, err, want)
			}
		})
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
