package smbtest

import (
	"bytes"
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestNewStorageUsesIndependentRealFilesystems(t *testing.T) {
	storage := NewStorage(t)
	other := NewStorage(t)
	root, err := storage.Lookup(t.Context(), "")
	if err != nil || !root.Exists || root.Attr.Kind != smb.KindDirectory {
		t.Fatalf("root: %+v, %v", root, err)
	}
	missing, err := storage.Lookup(t.Context(), "band")
	if err != nil || missing.Exists {
		t.Fatalf("missing file: %+v, %v", missing, err)
	}
	created, err := storage.Create(t.Context(), missing.Name, smb.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := storage.Open(t.Context(), created.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := storage.Close(context.WithoutCancel(t.Context()), writer); closeErr != nil {
			t.Error(closeErr)
		}
	})
	reader, err := storage.Open(t.Context(), created.Object, smb.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := storage.Close(context.WithoutCancel(t.Context()), reader); closeErr != nil {
			t.Error(closeErr)
		}
	})
	data := []byte("real file-backed storage")
	if n, writeErr := storage.WriteAt(t.Context(), writer, data, 0); writeErr != nil || n != len(data) {
		t.Fatalf("write: %d, %v", n, writeErr)
	}
	if flushErr := storage.Flush(t.Context(), reader, smb.SyncFull); flushErr != nil {
		t.Fatal(flushErr)
	}
	got := make([]byte, len(data))
	if n, readErr := storage.ReadAt(t.Context(), reader, got, 0); readErr != nil || n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("read: %q, %d, %v", got, n, readErr)
	}
	separate, err := other.Lookup(t.Context(), "band")
	if err != nil || separate.Exists {
		t.Fatalf("other runtime shared a file: %+v, %v", separate, err)
	}
}
