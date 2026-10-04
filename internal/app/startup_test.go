// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/storage"
)

type startupStore struct {
	object.ObjectStorage
	dir string
}

func (s startupStore) PutIfAbsent(_ context.Context, key string, r io.Reader) error {
	path := filepath.Join(s.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	return errors.Join(err, f.Close())
}

func TestMissingMarkerRequiresValidatedSnapshot(t *testing.T) {
	ctx := context.Background()
	remote := t.TempDir() + string(os.PathSeparator)
	raw, err := object.CreateStorage("file", remote, "", "", "")
	blob := startupStore{raw, remote}
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
	state := t.TempDir()
	path := filepath.Join(state, "metadata.db")
	m, err := storage.OpenMetadata(path, meta.DefaultConf())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	if err = m.Init(format, false); err != nil {
		t.Fatal(err)
	}
	p, err := backup.NewProtection(time.Hour, time.Minute, 14)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(m, blob, backup.Options{StateDir: state, DatabasePath: path, Interval: time.Hour, Timeout: time.Minute, Attempts: 1, Protection: p})
	if err != nil {
		t.Fatal(err)
	}
	r, err := manager.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyRemoteMarker(ctx, blob, format, false); err != nil {
		t.Fatalf("valid snapshot must supply missing marker identity: %v", err)
	}
	key := "meta/snapshot-" + r.Snapshot.Add(time.Second).UTC().Format("2006-01-02-150405") + ".db.gz"
	if err = blob.Put(ctx, key, strings.NewReader("corrupt newest point")); err != nil {
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
