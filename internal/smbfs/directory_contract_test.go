package smbfs

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

func assertDirectorySettings(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, setting := range []struct{ query, want string }{
		{"PRAGMA synchronous", "2"},
		{"PRAGMA fullfsync", "1"},
		{"PRAGMA checkpoint_fullfsync", "1"},
		{"PRAGMA journal_mode", "wal"},
		{"PRAGMA busy_timeout", "5000"},
	} {
		var value string
		if err := db.QueryRowContext(t.Context(), setting.query).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != setting.want {
			t.Fatalf("%s = %q, want %q", setting.query, value, setting.want)
		}
	}
}

func TestShutdownClosesOnlyDirectoryConnection(t *testing.T) {
	f := newFixture(t, 0)
	if err := f.fs.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.directory.PingContext(t.Context()); err == nil {
		t.Fatal("directory connection survived shutdown")
	}
	var root meta.Attr
	if eno := f.metadata.GetAttr(storageContext(t.Context()), meta.RootInode, &root); eno != 0 {
		t.Fatalf("caller-owned metadata closed: %v", eno)
	}
}

func TestDirectoryConnectionSettingsAndReuse(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "readonly"}[readOnly], func(t *testing.T) { testDirectoryConnection(t, readOnly) })
	}
}

func testDirectoryConnection(t *testing.T, readOnly bool) {
	t.Helper()
	f := newFixture(t, 0)
	for _, name := range []string{"first", "hidden", "last"} {
		f.create(t, name, smb.KindFile)
	}
	adapter := f.fs
	if readOnly {
		var err error
		adapter, err = New(Options{Filesystem: f.native, Barrier: f.fs.barrier, Config: f.config, Store: f.chunks, MetadataPath: f.path, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if shutdownErr := adapter.Shutdown(); shutdownErr != nil {
				t.Error(shutdownErr)
			}
		})
	}
	assertDirectorySettings(t, adapter.directory)
	// This view is connection-local. Pages reopening SQLite would see "hidden".
	if _, err := adapter.directory.ExecContext(t.Context(), `CREATE TEMP VIEW jfs_edge AS SELECT * FROM main.jfs_edge WHERE name != CAST('hidden' AS BLOB)`); err != nil {
		t.Fatal(err)
	}
	var cookie smb.Cookie
	for _, name := range []string{"first", "last"} {
		page, err := adapter.ReadDir(t.Context(), 1, cookie, 1)
		if err != nil || len(page) != 1 || page[0].Name != name {
			t.Fatalf("connection-local page = %+v, %v", page, err)
		}
		cookie = page[0].Next
	}
	stats := adapter.directory.Stats()
	if stats.MaxOpenConnections != 1 || stats.OpenConnections != 1 || stats.Idle != 1 {
		t.Fatalf("directory pool = %+v", stats)
	}
	conn, err := adapter.directory.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(any) error { return driver.ErrBadConn })
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("retire connection: %v", err)
	}
	if err = conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	assertDirectorySettings(t, adapter.directory)
	page, err := adapter.ReadDir(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 3 {
		t.Fatalf("replacement lost page or retained temp view: %+v, %v", page, err)
	}
}

type schemaColumn struct {
	sqlType             string
	notNull, primaryKey int
}

func TestPinnedDirectorySchema(t *testing.T) {
	f := newFixture(t, 0)
	for _, table := range []struct {
		columns map[string]schemaColumn
		query   string
	}{
		{query: "PRAGMA table_info(jfs_edge)", columns: map[string]schemaColumn{
			"id": {"INTEGER", 1, 1}, "parent": {"INTEGER", 1, 0}, "name": {"BLOB", 1, 0}, "inode": {"INTEGER", 1, 0},
		}},
		{query: "PRAGMA table_info(jfs_node)", columns: map[string]schemaColumn{
			"inode": {"INTEGER", 1, 1}, "type": {"INTEGER", 1, 0}, "length": {"INTEGER", 1, 0}, "parent": {"INTEGER", 0, 0},
			"atime": {"INTEGER", 1, 0}, "mtime": {"INTEGER", 1, 0}, "ctime": {"INTEGER", 1, 0},
			"atimensec": {"INTEGER", 1, 0}, "mtimensec": {"INTEGER", 1, 0}, "ctimensec": {"INTEGER", 1, 0},
		}},
		{query: "PRAGMA table_info(jfs_xattr)", columns: map[string]schemaColumn{
			"inode": {"INTEGER", 1, 0}, "name": {"TEXT", 1, 0}, "value": {"BLOB", 1, 0},
		}},
	} {
		assertSchemaColumns(t, f.fs.directory, table.query, table.columns)
	}
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	stamp := time.Unix(1700000000, 987654321).UTC()
	bits := uint32(0x21)
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Created: &stamp, Accessed: &stamp, Modified: &stamp, Changed: &stamp, Attributes: &bits}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Close(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	page, err := f.fs.ReadDir(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("schema page = %+v, %v", page, err)
	}
	a := page[0].Attr
	if a.Kind != smb.KindFile || a.Inode != r.Object.Inode || a.Size != 6 || a.Attributes != bits || !a.Created.Equal(stamp) || !a.Accessed.Equal(stamp) || !a.Modified.Equal(stamp) || !a.Changed.Equal(stamp) {
		t.Fatalf("schema decoding = %+v", a)
	}
}

func assertSchemaColumns(t *testing.T, db *sql.DB, query string, want map[string]schemaColumn) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	columns := make(map[string]schemaColumn)
	for rows.Next() {
		var position int
		var name string
		var column schemaColumn
		var defaultValue sql.NullString
		if err = rows.Scan(&position, &name, &column.sqlType, &column.notNull, &defaultValue, &column.primaryKey); err != nil {
			t.Error(err)
			break
		}
		columns[name] = column
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for name, expected := range want {
		if columns[name] != expected {
			t.Fatalf("%s: %s = %+v, want %+v", query, name, columns[name], expected)
		}
	}
}

func assertDirectoryIndex(t *testing.T, db *sql.DB) {
	t.Helper()
	var columns string
	if err := db.QueryRowContext(t.Context(), `SELECT group_concat(name,',') FROM pragma_index_info('IDX_jfs_edge_smbfs_directory_page')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != "parent,id" {
		t.Fatalf("directory index = %s", columns)
	}
}

func TestDirectoryIndexSurvivesSQLiteSnapshotRecovery(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "band", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	// Repeating startup preparation must be harmless.
	for range 2 {
		db, err := prepareDirectoryPages(f.path, false)
		if err != nil {
			t.Fatal(err)
		}
		assertDirectoryIndex(t, db)
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	restoredDir := t.TempDir()
	snapshot := filepath.Join(restoredDir, "meta.db")
	// P2 metadata backup uses a native SQLite VACUUM INTO snapshot, not DumpMeta.
	if _, err := f.fs.directory.ExecContext(t.Context(), "VACUUM INTO ?", snapshot); err != nil {
		t.Fatal(err)
	}
	image, err := directoryDB(snapshot, false)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectoryIndex(t, image)
	if err = image.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := fixtureAt(t, restoredDir, 0, false, 0)
	assertDirectoryIndex(t, recovered.fs.directory)
	page, err := recovered.fs.ReadDir(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 1 || page[0].Name != "band" || page[0].Attr.Inode != r.Object.Inode || page[0].Attr.Size != 6 {
		t.Fatalf("recovered indexed page = %+v, %v", page, err)
	}
}
