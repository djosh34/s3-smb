package smbfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestDiskBarrierDatabaseAndOptionalWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	if err := os.WriteFile(path, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	barrier, err := NewMetadataBarrier(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		if err = barrier.Commit(t.Context(), full); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path+"-wal", []byte("wal"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err = barrier.Commit(t.Context(), full); err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(path + "-wal"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireError(t, barrier.Commit(ctx, true), context.Canceled)
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	requireError(t, barrier.Commit(t.Context(), true), os.ErrNotExist)
}

func TestConstructorRejectsMissingDependencies(t *testing.T) {
	_, err := New(Options{})
	requireError(t, err, smb.ErrInvalidParameter)
	_, err = NewMetadataBarrier("relative.db")
	if err == nil {
		t.Fatal("accepted relative path")
	}
	_, err = NewMetadataBarrier(t.TempDir())
	if err == nil {
		t.Fatal("accepted directory")
	}
	_, err = NewMetadataBarrier(filepath.Join(t.TempDir(), "missing.db"))
	requireError(t, err, os.ErrNotExist)
}

func TestSyncModesAndStreamBarriers(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	called := false
	barrier := f.fs.barrier
	f.fs.barrier = testBarrier{commit: func(ctx context.Context, full bool) error { called = full; return barrier.Commit(ctx, full) }}
	for _, key := range []smb.ObjectKey{base.Object, stream.Object} {
		h := f.open(t, key, smb.AccessRead|smb.AccessWrite)
		write(t, f.fs, h, "bytes", 0)
		for _, mode := range []smb.SyncMode{smb.SyncData, smb.SyncFull} {
			if err := f.fs.Flush(t.Context(), h, mode); err != nil {
				t.Fatal(err)
			}
			if called != (mode == smb.SyncFull) {
				t.Fatal("wrong metadata barrier mode")
			}
		}
	}
}

func TestClosePropagatesFlushErrorAndReleasesReference(t *testing.T) {
	f := newFixture(t, 0)
	base := f.create(t, "data", smb.KindFile)
	h := f.open(t, base.Object, smb.AccessWrite)
	f.store.fail.Store(true)
	write(t, f.fs, h, "data", 0)
	err := f.fs.Close(t.Context(), h)
	if !errors.Is(err, smb.ErrIO) {
		t.Fatalf("close error = %v", err)
	}
	requireError(t, f.fs.Close(t.Context(), h), smb.ErrInvalidHandle)
	if len(f.fs.inodes) != 0 {
		t.Fatal("closed reference retained")
	}
}
