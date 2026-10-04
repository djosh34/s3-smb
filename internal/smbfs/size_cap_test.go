package smbfs

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestSizeCapNoopPreservesLengthAndMetadata(t *testing.T) {
	for _, path := range []string{"data", "data:fork"} {
		t.Run(path, func(t *testing.T) { checkSizeCapMetadata(t, path) })
	}
}

func checkSizeCapMetadata(t *testing.T, path string) {
	t.Helper()
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	selected := base
	if path != "data" {
		selected = f.create(t, path, smb.KindFile)
	}
	handle := f.open(t, selected.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, handle, "retained", 0)
	before, err := f.fs.GetAttr(t.Context(), selected.Object)
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range []uint64{8, 8192} {
		if setErr := f.fs.SetAttr(t.Context(), selected.Object, smb.AttrChange{SizeCap: &cap}); setErr != nil {
			t.Fatal(setErr)
		}
		after, attrErr := f.fs.GetAttr(t.Context(), selected.Object)
		if attrErr != nil || after != before {
			t.Fatalf("cap %d changed length or metadata: %+v, want %+v; %v", cap, after, before, attrErr)
		}
	}
	cap := uint64(3)
	if setErr := f.fs.SetAttr(t.Context(), selected.Object, smb.AttrChange{SizeCap: &cap}); setErr != nil {
		t.Fatal(setErr)
	}
	if flushErr := f.fs.Flush(t.Context(), handle, smb.SyncData); flushErr != nil {
		t.Fatal(flushErr)
	}
	read(t, f.fs, handle, []byte("ret"))
	if path != "data" {
		attr, attrErr := f.fs.GetAttr(t.Context(), base.Object)
		if attrErr != nil || attr.Size != 0 {
			t.Fatalf("stream cap changed base length: %+v, %v", attr, attrErr)
		}
	}
}

func TestSizeCapRejectsInvalidOrCanceledChanges(t *testing.T) {
	f := newFixture(t, 0)
	selected := f.create(t, "data", smb.KindFile)
	handle := f.open(t, selected.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, handle, "retained", 0)
	before, err := f.fs.GetAttr(t.Context(), selected.Object)
	if err != nil {
		t.Fatal(err)
	}
	cap, oversized := uint64(3), uint64(1<<63)
	for _, test := range []struct {
		change   smb.AttrChange
		want     error
		name     string
		canceled bool
	}{
		{smb.AttrChange{Size: &cap, SizeCap: &cap}, smb.ErrInvalidParameter, "absolute and capped", false},
		{smb.AttrChange{SizeCap: &oversized}, smb.ErrFileTooLarge, "oversized cap", false},
		{smb.AttrChange{SizeCap: &cap}, context.Canceled, "canceled cap", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			if test.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			requireError(t, f.fs.SetAttr(ctx, selected.Object, test.change), test.want)
			after, attrErr := f.fs.GetAttr(t.Context(), selected.Object)
			if attrErr != nil || after != before {
				t.Fatalf("rejected cap changed metadata: %+v, want %+v; %v", after, before, attrErr)
			}
		})
	}
	directory := f.create(t, "dir", smb.KindDirectory)
	for _, value := range []uint64{0, 8192} {
		requireError(t, f.fs.SetAttr(t.Context(), directory.Object, smb.AttrChange{SizeCap: &value}), smb.ErrIsDirectory)
	}
}
