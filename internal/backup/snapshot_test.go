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
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func TestSnapshotExcludesUncommittedChanges(t *testing.T) {
	ctx := context.Background()
	m, _ := newMetadata(t)
	db, err := openSnapshotDB(m.path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, db.Close)
	before := counter(t, m.path)
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
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("snapshot is not private", st, err)
	}
	if got := counter(t, path); got != before {
		t.Fatalf("snapshot contains an uncommitted counter: %d, want %d", got, before)
	}
	compressed := filepath.Join(t.TempDir(), "snapshot.db.gz")
	if err = compressSnapshot(path, compressed); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Clean(compressed))
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(gz, 1<<30))
	if err = errors.Join(err, gz.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	if gz.Comment != "sha256:"+hex.EncodeToString(h.Sum(nil)) {
		t.Fatal("snapshot has no matching database SHA-256")
	}
}

func counter(t *testing.T, path string) int64 {
	t.Helper()
	db, err := openSnapshotDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	var value int64
	err = db.QueryRowContext(context.Background(), "SELECT value FROM jfs_counter WHERE name='nextInode'").Scan(&value)
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestBackupOfMissingDatabaseCreatesAndUploadsNothing(t *testing.T) {
	m, _ := newMetadata(t)
	s := newStore(t)
	mgr := newManager(t, m, s, t.TempDir(), time.Now, time.Minute)
	missing := filepath.Join(t.TempDir(), "missing.db")
	mgr.opts.DatabasePath = missing
	mgr.wait = func(context.Context, time.Duration) error { return errors.New("no retry") }
	if _, err := mgr.Backup(context.Background()); err == nil {
		t.Fatal("backed up a missing database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("snapshot created the source database: %v", err)
	}
	if s.puts.Load() != 0 {
		t.Fatal("uploaded an incomplete snapshot")
	}
}

type corruptReadbackStore struct{ *fixtureStore }

func (corruptReadbackStore) Get(context.Context, string, int64, int64, ...object.AttrGetter) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("wrong stored bytes")), nil
}

func TestReadbackMismatchDoesNotOpenProtection(t *testing.T) {
	m, _ := newMetadata(t)
	state := t.TempDir()
	mgr := newManager(t, m, corruptReadbackStore{newStore(t)}, state, time.Now, time.Minute)
	mgr.wait = func(context.Context, time.Duration) error { return errors.New("no retry") }
	if _, err := mgr.Backup(context.Background()); err == nil || !strings.Contains(err.Error(), "readback mismatch") {
		t.Fatal("accepted a corrupt readback:", err)
	}
	if !errors.Is(mgr.opts.Protection.Check(), ErrUnprotected) {
		t.Fatal("readback failure left protection open")
	}
	if _, err := os.Stat(filepath.Join(state, "backup-receipt.json")); !os.IsNotExist(err) {
		t.Fatal("readback failure published a receipt:", err)
	}
	entries, err := os.ReadDir(filepath.Join(state, "backup-staging"))
	if err != nil || len(entries) != 0 {
		t.Fatal("failed backup left staging behind:", entries, err)
	}
}

func TestSnapshotStagingCleanupLeavesUnknownFiles(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "snapshot-owned")
	unknown := filepath.Join(root, "snapshot-unknown")
	for _, dir := range []string{owned, unknown} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"metadata.db", "snapshot.db.gz"} {
		if err := os.WriteFile(filepath.Join(owned, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(unknown, "unrelated")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "snapshot-link")
	if err := os.Symlink(unknown, link); err != nil {
		t.Fatal(err)
	}
	if err := cleanupSnapshotStaging(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatal("left an abandoned snapshot:", err)
	}
	if data, err := os.ReadFile(filepath.Clean(keep)); err != nil || string(data) != "keep" {
		t.Fatal("removed an unknown staging file:", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("removed an unknown staging symlink:", err)
	}
}
