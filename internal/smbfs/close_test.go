package smbfs

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCloseCanceledContextStillReleases(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "data", 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireError(t, f.fs.Close(ctx, h), context.Canceled)
	requireError(t, f.fs.Close(t.Context(), h), smb.ErrInvalidHandle)
	if len(f.fs.inodes) != 0 {
		t.Fatal("canceled close retained native reference")
	}
}
