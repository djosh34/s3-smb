// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotWhileWriterHasUncommittedChanges(t *testing.T) {
	m, _ := newMetadata(t)
	ctx := context.Background()
	db, err := openSnapshotDB(m.path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	var before int64
	if err = db.QueryRowContext(ctx, "SELECT value FROM jfs_counter WHERE name='nextInode'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE jfs_counter SET value=value+100 WHERE name='nextInode'"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot's name.db")
	if err = takeSnapshot(ctx, m.path, path); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("snapshot is not private", st, err)
	}
	snapshot, err := openSnapshotDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	var got int64
	if err = snapshot.QueryRowContext(ctx, "SELECT value FROM jfs_counter WHERE name='nextInode'").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != before {
		t.Fatalf("snapshot contains uncommitted counter: %d, want %d", got, before)
	}
	compressed := filepath.Join(t.TempDir(), "snapshot.db.gz")
	if err = compressSnapshot(path, compressed); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(compressed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err = io.Copy(h, gz); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	if gz.Comment != "sha256:"+hex.EncodeToString(h.Sum(nil)) {
		t.Fatal("snapshot has no matching database SHA-256")
	}
}

func TestSnapshotMissingSourceDoesNotCreateDatabase(t *testing.T) {
	source := filepath.Join(t.TempDir(), "missing.db")
	target := filepath.Join(t.TempDir(), "snapshot.db")
	if err := takeSnapshot(context.Background(), source, target); err == nil {
		t.Fatal("accepted missing source")
	}
	for _, path := range []string{source, target} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("created %s: %v", path, err)
		}
	}
}

func TestRetentionAndReservationCleanup(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	mgr := newManager(t, m, s, t.TempDir(), func() time.Time { return now }, time.Minute)
	if err := mgr.opts.Protection.protect(now); err != nil {
		t.Fatal(err)
	}
	key := func(age time.Duration) string {
		return "meta/snapshot-" + now.Add(-age).Format("2006-01-02-150405") + ".db.gz"
	}
	// Earliest snapshot in each thinning period wins, not the newest.
	kept := []string{key(time.Hour), key(47 * time.Hour), key(71 * time.Hour), key(335 * time.Hour), key(600 * time.Hour), key(2000 * time.Hour)}
	removed := []string{key(49 * time.Hour), key(313 * time.Hour), key(550 * time.Hour), key(1900 * time.Hour), key(3 * 365 * 24 * time.Hour)}
	for _, keys := range [][]string{kept, removed} {
		for _, k := range keys {
			if err := s.Put(context.Background(), k, nilReader{}); err != nil {
				t.Fatal(err)
			}
			if err := mgr.reserve(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	oldFailed, recentFailed := key(100*time.Hour), key(2*time.Hour)
	for _, k := range []string{oldFailed, recentFailed} {
		if err := mgr.reserve(k); err != nil {
			t.Fatal(err)
		}
	}
	unknown := filepath.Join(mgr.opts.StateDir, "backup-names", "keep-me")
	if err := os.WriteFile(unknown, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.cleanup(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	for _, k := range kept {
		if _, err := s.Head(context.Background(), k); err != nil {
			t.Fatalf("removed retained snapshot %s: %v", k, err)
		}
		if _, err := os.Stat(filepath.Join(mgr.opts.StateDir, "backup-names", filepath.Base(k))); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range removed {
		if _, err := s.Head(context.Background(), k); !os.IsNotExist(err) {
			t.Fatalf("kept expired snapshot %s: %v", k, err)
		}
	}
	for _, k := range append(removed, oldFailed) {
		if _, err := os.Stat(filepath.Join(mgr.opts.StateDir, "backup-names", filepath.Base(k))); !os.IsNotExist(err) {
			t.Fatalf("kept expired reservation %s: %v", k, err)
		}
	}
	for _, path := range []string{unknown, filepath.Join(mgr.opts.StateDir, "backup-names", filepath.Base(recentFailed))} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	mgr.opts.Protection.Close()
	if err := mgr.cleanup(context.Background(), now.AddDate(3, 0, 0)); !errors.Is(err, ErrUnprotected) {
		t.Fatalf("retention ignored protection: %v", err)
	}
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }
