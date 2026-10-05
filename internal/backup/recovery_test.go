// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
)

// republish replaces a snapshot with data under the given recorded hash.
func republish(t *testing.T, s *fixtureStore, key string, data []byte, digest string) {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Comment = "sha256:" + digest
	_, err := gz.Write(data)
	if err = errors.Join(err, gz.Close()); err != nil {
		t.Fatal(err)
	}
	if err = s.Put(context.Background(), key, &compressed); err != nil {
		t.Fatal(err)
	}
}

// corrupt replaces the snapshot at key with one that fails the given check.
func corrupt(t *testing.T, s *fixtureStore, key, failure string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.db")
	if err := downloadSnapshot(t.Context(), s, key, path); err != nil {
		t.Fatal(err)
	}
	if failure == "integrity_check" {
		// Point a table at a missing page without touching the SQLite
		// header. The gzip checksum and recorded SHA-256 stay valid.
		db, err := openSnapshotDB(path, "rw")
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(t.Context(), "PRAGMA writable_schema=ON; UPDATE sqlite_master SET rootpage=999999 WHERE name='jfs_node'")
		if err = errors.Join(err, db.Close()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	if failure == "SHA-256" {
		digest = strings.Repeat("0", 64)
	}
	republish(t, s, key, data, digest)
}

func TestRecoveryRejectsInvalidSnapshots(t *testing.T) {
	for _, failure := range []string{"SHA-256", "integrity_check", "identity"} {
		t.Run(failure, func(t *testing.T) {
			ctx := t.Context()
			m, f := newMetadata(t)
			s := newStore(t)
			r := backupOnce(t, m, s)
			if failure == "identity" {
				changed := *f
				changed.Name = "another-volume"
				f = &changed
			} else {
				corrupt(t, s, r.Key, failure)
			}
			// Validation fails before the volume cache is touched.
			cache := t.TempDir()
			keep := filepath.Join(cache, f.UUID, "keep")
			if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keep, []byte("old cache"), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if _, err := Recover(ctx, s, r.Key, filepath.Join(dir, "restored.db"), cache, f); err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("wanted %s refusal, got %v", failure, err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("failed recovery left a database or staging: %v %v", entries, err)
			}
			if _, err := os.Stat(keep); err != nil {
				t.Fatal("failed recovery changed the cache:", err)
			}
			if failure != "identity" {
				if _, err := Inspect(ctx, s, r.Key, t.TempDir()); err == nil || !strings.Contains(err.Error(), failure) {
					t.Fatalf("inspection did not check %s: %v", failure, err)
				}
			}
		})
	}
}

func TestInspectionRequiresAbsoluteStateDirectory(t *testing.T) {
	if _, err := Inspect(context.Background(), newStore(t), "meta/snapshot-2026-01-01-000000.db.gz", "relative"); err == nil {
		t.Fatal("inspection accepted a relative state directory")
	}
}

func TestRecoveryClearsLiveLocksAndSessions(t *testing.T) {
	m, f := newMetadata(t)
	ino := createInode(t, m, "locked")
	sustained := createInode(t, m, "unlinked-open")
	if err := m.NewSession(true); err != nil {
		t.Fatal(err)
	}
	if st := m.Unlink(meta.Background(), meta.RootInode, "unlinked-open", true); st != 0 {
		t.Fatal(st)
	}
	if count := queryCount(t, m.path, "jfs_sustained"); count != 1 {
		t.Fatalf("fixture has %d sustained inodes", count)
	}
	if st := m.Flock(meta.Background(), ino, 1, meta.F_WRLCK, false); st != 0 {
		t.Fatal(st)
	}
	if st := m.Setlk(meta.Background(), ino, 1, false, meta.F_WRLCK, 0, 100, 1); st != 0 {
		t.Fatal(st)
	}
	s := newStore(t)
	r := backupOnce(t, m, s)
	// The old writer has stopped, but its snapshot still holds its
	// session and both kinds of locks.
	if err := m.CloseSession(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Recover(context.Background(), s, r.Key, path, t.TempDir(), f); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"jfs_flock", "jfs_plock"} {
		if count := queryCount(t, path, table); count != 0 {
			t.Fatalf("%d restored rows in %s", count, table)
		}
	}
	restored := openMetadata(t, path, true, 0)
	if err := restored.NewSession(true); err != nil {
		t.Fatal(err)
	}
	var attr meta.Attr
	if restored.GetAttr(meta.Background(), sustained, &attr) != syscall.ENOENT {
		t.Fatal("unlinked open inode survived its dead session")
	}
	var newIno meta.Ino
	if st := restored.Create(meta.Background(), meta.RootInode, "forbidden", 0o600, 0, 0, &newIno, &attr); st != syscall.EROFS {
		t.Fatalf("recovery weakened read-only mode: %v", st)
	}
}

func TestRecoveryCleansMoreThanOneSessionBatch(t *testing.T) {
	m, f := newMetadata(t)
	db, err := openSnapshotDB(m.path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `WITH RECURSIVE ids(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<1001
	) INSERT INTO jfs_session2(sid, expire, info) SELECT n, ?, '{}' FROM ids`, time.Now().Add(time.Hour).Unix())
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	r := backupOnce(t, m, s)
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(context.Background(), s, r.Key, path, t.TempDir(), f); err != nil {
		t.Fatal(err)
	}
	if count := queryCount(t, path, "jfs_session2"); count != 0 {
		t.Fatalf("%d sessions left", count)
	}
}

func TestRecoveryRefusesSustainedRowsWithoutSession(t *testing.T) {
	m, f := newMetadata(t)
	ino := createInode(t, m, "orphan-sustained")
	db, err := openSnapshotDB(m.path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), "INSERT INTO jfs_sustained(sid, inode) VALUES(999, ?)", ino)
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	r := backupOnce(t, m, s)
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(context.Background(), s, r.Key, path, t.TempDir(), f); err == nil || !strings.Contains(err.Error(), "jfs_sustained table is not empty") {
		t.Fatal("published recovery with open-file rows left:", err)
	}
}

// stalledSessions makes CleanStaleSessions do nothing. JuiceFS only logs its
// cleanup errors, so recovery must notice a cleanup without progress.
type stalledSessions struct{ meta.Meta }

func (stalledSessions) CleanStaleSessions(meta.Context) {}

func TestSessionCleanupWithoutProgressFails(t *testing.T) {
	m, _ := newMetadata(t)
	if err := m.NewSession(true); err != nil {
		t.Fatal(err)
	}
	cleanup(t, m.CloseSession)
	if err := cleanSnapshotSessions(context.Background(), stalledSessions{m}, m.path); err == nil {
		t.Fatal("accepted a session cleanup without progress")
	}
}

func TestRecoveryCacheWipeFailureDoesNotPublish(t *testing.T) {
	m, format := newMetadata(t)
	s := newStore(t)
	r := backupOnce(t, m, s)
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.WriteFile(cache, []byte("unrelated file"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "metadata.db")
	if _, err := Recover(context.Background(), s, r.Key, db, cache, format); err == nil {
		t.Fatal("published a database after a failed cache wipe")
	}
	if _, err := os.Lstat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database exists after a failed wipe: %v", err)
	}
	if got, err := os.ReadFile(filepath.Clean(cache)); err != nil || string(got) != "unrelated file" {
		t.Fatalf("cache root file changed: %q, %v", got, err)
	}
}

func TestCleanupRecoveryStagingLeavesUnknownFiles(t *testing.T) {
	dir := t.TempDir()
	owned := filepath.Join(dir, ".s3-smb-recovery-owned")
	unknown := filepath.Join(dir, ".s3-smb-recovery-unknown")
	linked := filepath.Join(dir, ".s3-smb-recovery-linked")
	other := filepath.Join(dir, "someone-elses-directory")
	for _, p := range []string{owned, unknown, other} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{
		filepath.Join(owned, "metadata.db"), filepath.Join(owned, "metadata.db-wal"), filepath.Join(owned, "metadata.db-shm"),
		filepath.Join(unknown, "do-not-delete"), filepath.Join(other, "metadata.db"),
	}
	for _, p := range files {
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, linked); err != nil {
		t.Fatal(err)
	}
	if err := CleanupRecoveryStaging(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("abandoned staging not removed: %v", err)
	}
	for _, p := range append(files[3:], linked) {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("unknown file removed: %s %v", p, err)
		}
	}
}
