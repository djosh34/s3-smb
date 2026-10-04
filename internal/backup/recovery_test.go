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

func createInode(t *testing.T, m meta.Meta, name string) meta.Ino {
	t.Helper()
	var ino meta.Ino
	var attr meta.Attr
	if st := m.Create(meta.Background(), meta.RootInode, name, 0644, 0, 0, &ino, &attr); st != 0 {
		t.Fatal(st)
	}
	return ino
}

func queryCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := openSnapshotDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var count int
	if err = db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func recoveredMetadata(t *testing.T, path string, readonly bool, deletes int) *testMetadata {
	t.Helper()
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.ReadOnly = readonly
	conf.MaxDeletes = deletes
	m, err := meta.NewSQLite(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(m.CloseSession(), m.Shutdown()); err != nil {
			t.Error(err)
		}
	})
	if _, err = m.Load(true); err != nil {
		t.Fatal(err)
	}
	return &testMetadata{m, path}
}

func TestRecoveryRejectsSHA256AndIntegrityFailure(t *testing.T) {
	for _, failure := range []string{"SHA-256", "integrity_check", "identity"} {
		t.Run(failure, func(t *testing.T) {
			m, f := newMetadata(t)
			s := newStore(t)
			mgr := newManager(t, m, s, t.TempDir(), time.Now, time.Minute)
			r, err := mgr.Backup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if failure != "identity" {
				path := filepath.Join(t.TempDir(), "metadata.db")
				if err = downloadSnapshot(context.Background(), s, r.Key, path); err != nil {
					t.Fatal(err)
				}
				if failure == "integrity_check" {
					// Change a table's declared root page without damaging the SQL
					// header. The gzip checksum and recorded SHA-256 remain valid.
					db, err := openSnapshotDB(path, "rw")
					if err != nil {
						t.Fatal(err)
					}
					_, err = db.ExecContext(context.Background(), "PRAGMA writable_schema=ON; UPDATE sqlite_master SET rootpage=999999 WHERE name='jfs_node'")
					if err = errors.Join(err, db.Close()); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				h := sha256.Sum256(data)
				var compressed bytes.Buffer
				gz := gzip.NewWriter(&compressed)
				gz.Comment = "sha256:" + hex.EncodeToString(h[:])
				if failure == "SHA-256" {
					gz.Comment = "sha256:" + strings.Repeat("0", 64)
				}
				if _, err = gz.Write(data); err != nil {
					t.Fatal(err)
				}
				if err = gz.Close(); err != nil {
					t.Fatal(err)
				}
				if err = s.Put(context.Background(), r.Key, &compressed); err != nil {
					t.Fatal(err)
				}
			} else {
				changed := *f
				changed.UUID = "another-volume"
				f = &changed
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "restored.db")
			if _, err = Recover(context.Background(), s, r.Key, path, f); err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("wanted %s refusal, got %v", failure, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed recovery left database or staging: %v %v", entries, err)
			}
			if failure != "identity" {
				if _, err = Inspect(context.Background(), s, r.Key, t.TempDir()); err == nil || !strings.Contains(err.Error(), failure) {
					t.Fatalf("inspection did not validate %s: %v", failure, err)
				}
			}
		})
	}
}

func TestSnapshotOfLiveLocksRecoversToReadOnly(t *testing.T) {
	for _, readonlyWriter := range []bool{false, true} {
		name := "writable-session"
		if readonlyWriter {
			name = "readonly-sid-zero"
		}
		t.Run(name, func(t *testing.T) {
			m, f := newMetadata(t)
			ino := createInode(t, m, "locked")
			sustained := createInode(t, m, "unlinked-open")
			if !readonlyWriter {
				if err := m.NewSession(true); err != nil {
					t.Fatal(err)
				}
				if st := m.Unlink(meta.Background(), meta.RootInode, "unlinked-open", true); st != 0 {
					t.Fatal(st)
				}
				if count := queryCount(t, m.path, "jfs_sustained"); count != 1 {
					t.Fatalf("fixture has %d sustained inodes", count)
				}
			} else {
				m = recoveredMetadata(t, m.path, true, 0)
				if err := m.NewSession(true); err != nil {
					t.Fatal(err)
				}
			}
			if st := m.Flock(meta.Background(), ino, 1, meta.F_WRLCK, false); st != 0 {
				t.Fatal(st)
			}
			if st := m.Setlk(meta.Background(), ino, 1, false, meta.F_WRLCK, 0, 100, 1); st != 0 {
				t.Fatal(st)
			}
			s := newStore(t)
			r, err := newManager(t, m, s, t.TempDir(), time.Now, time.Minute).Backup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			// The old writer is stopped before recovery, but its snapshot still
			// contains the valid session and both kinds of locks.
			if err = m.CloseSession(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "restored.db")
			if _, err = Recover(context.Background(), s, r.Key, path, f); err != nil {
				t.Fatal(err)
			}
			if err = checkSnapshotSessions(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			restored := recoveredMetadata(t, path, true, 0)
			if err = restored.NewSession(true); err != nil {
				t.Fatal(err)
			}
			if st := restored.Flock(meta.Background(), ino, 2, meta.F_WRLCK, false); st != 0 {
				t.Fatalf("restored flock remains: %v", st)
			}
			if st := restored.Setlk(meta.Background(), ino, 2, false, meta.F_WRLCK, 0, 100, 2); st != 0 {
				t.Fatalf("restored byte-range lock remains: %v", st)
			}
			var attr meta.Attr
			if !readonlyWriter && restored.GetAttr(meta.Background(), sustained, &attr) != syscall.ENOENT {
				t.Fatal("unlinked open inode survived its dead session")
			}
			var newIno meta.Ino
			if st := restored.Create(meta.Background(), meta.RootInode, "forbidden", 0600, 0, 0, &newIno, &attr); st != syscall.EROFS {
				t.Fatalf("recovery weakened readonly mode: %v", st)
			}
		})
	}
}
