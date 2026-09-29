// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/thirdparty/xorm"
	"github.com/mattn/go-sqlite3"
)

// A real OS write failure during native SQLite WAL COMMIT. RLIMIT_FSIZE is
// process-wide, so this MUST run in a subprocess, never in the parallel suite's
// own process. No fake driver, custom SQLite VFS or production test hook is used.
// It is not a power-loss or isolated fsync-failure simulation.
func TestSQLiteCommitIOFailurePropagates(t *testing.T) {
	if os.Getenv("S3_SMB_TEST_SQLITE_COMMIT_IO_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteCommitIOFailurePropagates$", "-test.v", "-test.timeout=25s")
		cmd.Env = append(os.Environ(), "S3_SMB_TEST_SQLITE_COMMIT_IO_CHILD=1", "GOMAXPROCS=2")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("native SQLite I/O subprocess: %v\n%s", err, out)
		}
		if !bytes.Contains(out, []byte("statement succeeded; native COMMIT rejected SQLITE_IOERR_WRITE")) {
			t.Fatalf("missing actual commit-failure evidence:\n%s", out)
		}
		t.Logf("%s", out)
		return
	}
	path := filepath.Join(t.TempDir(), "metadata.db")
	conf := DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	mm, err := NewSQLite(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	m := mm.(*dbMeta)
	defer m.Shutdown()
	m.db.DB().SetMaxOpenConns(1)
	m.db.DB().SetMaxIdleConns(1)
	if err = m.Init(&Format{Name: "io-test", UUID: "io-test-id", TrashDays: 14, BlockSize: 4096}, false); err != nil {
		t.Fatal(err)
	}
	const key = "user.commit-proof"
	const before = "committed-before-I/O-failure"
	if st := m.SetXattr(Background(), RootInode, key, []byte(before), 0); st != 0 {
		t.Fatal(st)
	}
	var syncMode int
	if err = m.db.DB().DB.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil || syncMode != 2 {
		t.Fatalf("FULL prerequisite: mode=%d err=%v", syncMode, err)
	}
	var busy, logFrames, checkpointed int
	if err = m.db.DB().DB.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil || busy != 0 {
		t.Fatalf("WAL checkpoint prerequisite: busy=%d err=%v", busy, err)
	}
	if st, err := os.Stat(path + "-wal"); err != nil || st.Size() != 0 {
		t.Fatalf("WAL must be empty before bound: %v %v", st, err)
	}
	var original syscall.Rlimit
	if err = syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	bound := original
	bound.Cur = 0
	if err = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &bound); err != nil {
		t.Fatal(err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original)
		}
	}()
	statementSucceeded := false
	err = m.txn(func(s *xorm.Session) error {
		n, e := s.Where("inode = ? AND name = ?", RootInode, key).Cols("value").Update(&xattr{Value: []byte("must-not-commit")})
		if e != nil {
			return e
		}
		if n != 1 {
			return errors.New("fixture row not updated")
		}
		statementSucceeded = true
		return nil // native Xorm must propagate the following COMMIT write failure
	})
	var sqlErr sqlite3.Error
	if !statementSucceeded || !errors.As(err, &sqlErr) || sqlErr.Code != sqlite3.ErrIoErr || sqlErr.ExtendedCode != sqlite3.ErrIoErrWrite {
		t.Fatalf("expected real commit-time SQLite WAL write error, statement=%v err=%T %v", statementSucceeded, err, err)
	}
	t.Log("statement succeeded; native COMMIT rejected SQLITE_IOERR_WRITE")
	// Exercise the actual public metadata mutation, not only the SQL helper.
	// With the same OS fault, it must not acknowledge successful xattr storage.
	if st := m.SetXattr(Background(), RootInode, key, []byte("must-not-acknowledge"), 0); st != syscall.EIO {
		t.Fatalf("native SetXattr acknowledged/changed SQLite I/O failure: %v", st)
	}
	var value []byte
	if st := m.GetXattr(Background(), RootInode, key, &value); st != 0 || string(value) != before {
		t.Fatalf("failed transaction changed existing metadata: %q %v", value, st)
	}
	if err = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	restored = true
	if err = m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// Cold reopen proves the earlier committed state survived; no success is
	// inferred merely from an in-memory rollback or mocked upload failure.
	reopened, err := NewSQLite(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Shutdown()
	if _, err = reopened.Load(false); err != nil {
		t.Fatal(err)
	}
	value = nil
	if st := reopened.GetXattr(Background(), RootInode, key, &value); st != 0 || string(value) != before {
		t.Fatalf("cold reopen lost earlier commit: %q %v", value, st)
	}
}
