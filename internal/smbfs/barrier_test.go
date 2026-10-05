package smbfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestConstructorRejectsInvalidOptions(t *testing.T) {
	_, err := New(Options{})
	requireError(t, err, smb.ErrInvalidParameter)
	f := newFixture(t, 0)
	_, err = New(Options{Filesystem: f.native, Barrier: f.fs.barrier, Config: f.config, Store: f.chunks, MetadataPath: f.path, ReadRetryWindow: -time.Second})
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
