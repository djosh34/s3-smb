// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestSQLiteFullFSyncEveryConnection(t *testing.T) {
	for _, entry := range []string{"embedding", "native"} {
		t.Run(entry, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.db")
			var mm Meta
			var err error
			if entry == "embedding" {
				mm, err = NewSQLite(path, DefaultConf())
			} else {
				mm, err = newSQLMeta("sqlite3", path, DefaultConf())
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := mm.Shutdown(); err != nil {
					t.Error(err)
				}
			})
			if mm.Name() != "sqlite3" {
				t.Fatalf("engine name = %q, want sqlite3", mm.Name())
			}
			m, ok := mm.(*dbMeta)
			if !ok {
				t.Fatalf("metadata type = %T, want *dbMeta", mm)
			}
			db := m.db.DB().DB
			db.SetMaxOpenConns(4)
			db.SetMaxIdleConns(0)
			for round := 0; round < 2; round++ {
				t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
					// Hold every connection until the pool is full. Cleanup
					// closes them, so the next round opens four replacements.
					for i := 0; i < 4; i++ {
						conn, err := db.Conn(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := conn.Close(); err != nil {
								t.Error(err)
							}
						})
						checkFullFSync(t, conn)
					}
					if n := db.Stats().OpenConnections; n != 4 {
						t.Fatalf("open connections = %d, want 4", n)
					}
				})
			}
		})
	}
}

func checkFullFSync(t *testing.T, conn *sql.Conn) {
	t.Helper()
	for _, pragma := range []string{"fullfsync", "checkpoint_fullfsync"} {
		var value int
		if err := conn.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&value); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
		if value != 1 {
			t.Fatalf("%s = %d, want 1", pragma, value)
		}
	}
}
