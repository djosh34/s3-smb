// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/storage"
)

func TestMissingMarkerRequiresValidatedNativeBackup(t *testing.T) {
	ctx := context.Background()
	blob, err := object.CreateStorage("file", t.TempDir()+string(os.PathSeparator), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	format, err := storage.NewFormat("s3-smb", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyRemoteMarker(ctx, blob, format, false); err == nil {
		t.Fatal("accepted markerless state without a backup")
	}
	m, err := storage.OpenMetadata(filepath.Join(t.TempDir(), "metadata.db"), meta.DefaultConf())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	if err = m.Init(format, false); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	if err = m.DumpMeta(gz, 0, 2, false, true, false); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err = blob.Put(ctx, "meta/dump-2026-01-01-000000.json.gz", bytes.NewReader(data.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err = verifyRemoteMarker(ctx, blob, format, false); err != nil {
		t.Fatalf("valid native backup must supply missing marker identity: %v", err)
	}
	if err = blob.Put(ctx, "meta/dump-2026-01-02-000000.json.gz", strings.NewReader("corrupt newest point")); err != nil {
		t.Fatal(err)
	}
	if err = verifyRemoteMarker(ctx, blob, format, false); err == nil {
		t.Fatal("silently fell back from corrupt newest backup")
	}
	if err = blob.Put(ctx, "juicefs_uuid", strings.NewReader("different-volume")); err != nil {
		t.Fatal(err)
	}
	if err = verifyRemoteMarker(ctx, blob, format, true); err == nil {
		t.Fatal("validated backup bypassed a contradictory marker")
	}
}

func TestLocalMetadataNeverTreatsPartialStateAsAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.db")
	if exists, err := localMetadataExists(path); err != nil || exists {
		t.Fatalf("absent: %v %v", exists, err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := localMetadataExists(path); err == nil {
		t.Fatal("empty local file was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := localMetadataExists(path); err == nil {
		t.Fatal("dangling local symlink was accepted")
	}
}

func TestNativeIdentityComparisonIncludesDataLayoutAndKey(t *testing.T) {
	a, err := storage.NewFormat("s3-smb", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*meta.Format){
		func(f *meta.Format) { f.UUID = "another" }, func(f *meta.Format) { f.Name = "another" },
		func(f *meta.Format) { f.BlockSize *= 2 }, func(f *meta.Format) { f.Compression = "lz4" },
		func(f *meta.Format) { f.HashPrefix = !f.HashPrefix }, func(f *meta.Format) { f.Shards++ },
		func(f *meta.Format) { f.EncryptAlgo = "other" }, func(f *meta.Format) { f.EncryptKey = "other" },
	} {
		b := *a
		change(&b)
		if backup.SameVolume(a, &b) {
			t.Fatal("accepted changed native identity/data layout")
		}
	}
	b := *a
	b.Bucket = "old-endpoint"
	b.AccessKey = "old-credential"
	b.TrashDays++
	if !backup.SameVolume(a, &b) {
		t.Fatal("connection settings are not native volume identity")
	}
}
