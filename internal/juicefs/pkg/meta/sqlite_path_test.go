// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewSQLiteLiteralFilesystemPath(t *testing.T) {
	for _, name := range []string{"locked?ignored=1#literal", "locked#literal", "locked%3Fwith space"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			requested := filepath.Join(dir, "metadata?mode=memory#literal.db")
			// The app precreates this exact private path under its locked state dir.
			file, err := os.OpenFile(requested, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			m, err := NewSQLite(requested, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Shutdown()
			db := m.(*dbMeta).db.DB().DB
			rows, err := db.Query("PRAGMA database_list")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var main string
			for rows.Next() {
				var seq int
				var schema, path string
				if err = rows.Scan(&seq, &schema, &path); err != nil {
					t.Fatal(err)
				}
				if schema == "main" {
					main = path
				}
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			if main != requested {
				t.Fatalf("SQLite opened outside requested authority path: got %q want %q", main, requested)
			}
			var synchronous int
			if err = db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
				t.Fatalf("FULL lost: synchronous=%d err=%v", synchronous, err)
			}
			if err = m.Init(&Format{Name: "path-test", UUID: "path-test-id", TrashDays: 14, BlockSize: 4096}, false); err != nil {
				t.Fatal(err)
			}
			if st, err := os.Stat(requested); err != nil || st.Size() == 0 {
				t.Fatalf("literal precreated database not used: %v %v", st, err)
			}
		})
	}
}
