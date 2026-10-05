// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Carried over from the JuiceFS hook: every connection, including pool
// replacements, has full fsync on.
func TestSQLiteFullFSyncEveryConnection(t *testing.T) {
	db, err := openDatabase(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(0)
	for round := range 2 {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			// Hold every connection until the pool is full. Cleanup closes
			// them, so the next round opens four replacements.
			for range 4 {
				conn, err := db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
				})
				checkPragmas(t, conn)
			}
			if n := db.Stats().OpenConnections; n != 4 {
				t.Fatalf("open connections = %d, want 4", n)
			}
		})
	}
}

func checkPragmas(t *testing.T, conn *sql.Conn) {
	t.Helper()
	for pragma, want := range map[string]string{"fullfsync": "1", "checkpoint_fullfsync": "1", "synchronous": "2", "journal_mode": "wal"} {
		var value string
		if err := conn.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&value); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
		if value != want {
			t.Fatalf("%s = %s, want %s", pragma, value, want)
		}
	}
}

func TestDatabaseFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	db, err := openDatabase(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `UPDATE state SET commits = commits + 1`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{databaseName, databaseName + "-wal"} {
		info, statErr := os.Stat(filepath.Join(dir, name))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", name, info.Mode().Perm())
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}
