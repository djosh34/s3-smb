// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
)

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
	r, err := newManager(t, m, s, t.TempDir(), time.Now, time.Minute).Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(context.Background(), s, r.Key, path, f); err != nil {
		t.Fatal(err)
	}
	if err = checkSnapshotSessions(context.Background(), path); err != nil {
		t.Fatal(err)
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
	r, err := newManager(t, m, s, t.TempDir(), time.Now, time.Minute).Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(context.Background(), s, r.Key, path, f); err == nil || !strings.Contains(err.Error(), "jfs_sustained table is not empty") {
		t.Fatal("published recovery with uncleared open-file rows", err)
	}
}

// CleanStaleSessions does not return its native cleanup errors. Recovery must
// notice a cleanup that makes no progress rather than loop or publish it.
type stalledSessions struct{ meta.Meta }

func (stalledSessions) CleanStaleSessions(meta.Context) {}

func TestSessionCleanupWithoutProgressFails(t *testing.T) {
	m, _ := newMetadata(t)
	if err := m.NewSession(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.CloseSession(); err != nil {
			t.Error(err)
		}
	})
	if err := cleanSnapshotSessions(context.Background(), stalledSessions{m}, m.path); err == nil {
		t.Fatal("accepted unsuccessful native session cleanup")
	}
}
