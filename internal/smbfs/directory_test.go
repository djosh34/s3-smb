package smbfs

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

func TestDirectoryCookieSurvivesRemoval(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	a := f.create(t, "dir/a", smb.KindFile)
	for _, name := range []string{"b", "c", "d"} {
		f.create(t, "dir/"+name, smb.KindFile)
	}
	page, err := f.fs.ReadDir(t.Context(), dir.Object.Inode, 0, 2)
	if err != nil || len(page) != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	cookie := page[1].Next
	if err = f.fs.Remove(t.Context(), a.Name, a.Object.Inode); err != nil {
		t.Fatal(err)
	}
	page, err = f.fs.ReadDir(t.Context(), dir.Object.Inode, cookie, 2)
	if err != nil || len(page) != 2 || page[0].Name != "c" || page[1].Name != "d" {
		t.Fatalf("continuation skipped entries: %+v, %v", page, err)
	}
	page, err = f.fs.ReadDir(t.Context(), dir.Object.Inode, page[1].Next, 2)
	if err != nil || len(page) != 0 {
		t.Fatalf("exhaustion = %+v, %v", page, err)
	}
}

func TestReadDirWithConcurrentRemoval(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	var removed []smb.Resolved
	for i := range 80 {
		r := f.create(t, fmt.Sprintf("dir/band-%02d", i), smb.KindFile)
		if i%2 == 0 {
			removed = append(removed, r)
		}
	}
	done := make(chan error, 1)
	go func() {
		for _, r := range removed {
			if eno := f.metadata.Unlink(storageContext(t.Context()), meta.Ino(r.Name.Parent), r.Name.Base); eno != 0 {
				done <- eno
				return
			}
		}
		done <- nil
	}()
	seen := make(map[string]bool)
	var cookie smb.Cookie
	for {
		page, err := f.fs.ReadDir(t.Context(), dir.Object.Inode, cookie, 7)
		if err != nil {
			t.Error(err)
			break
		}
		if len(page) == 0 {
			break
		}
		for _, entry := range page {
			seen[entry.Name] = true
			cookie = entry.Next
		}
	}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 80; i += 2 {
		if name := fmt.Sprintf("band-%02d", i); !seen[name] {
			t.Errorf("skipped live entry %s", name)
		}
	}
}

// A page is one indexed SQL query. Entries are reread only when their own
// inode changed since the page was read, not after unrelated I/O.
func TestDirectoryPageQueries(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	for i := range 256 {
		f.create(t, fmt.Sprintf("dir/band-%04d", i), smb.KindFile)
	}
	first, err := f.fs.Lookup(t.Context(), "dir/band-0000")
	if err != nil {
		t.Fatal(err)
	}
	h := f.open(t, first.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	if err = f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	want, err := f.fs.GetAttr(t.Context(), first.Object)
	if err != nil {
		t.Fatal(err)
	}
	other := f.create(t, "other", smb.KindFile)
	otherHandle := f.open(t, other.Object, smb.AccessWrite)
	write(t, f.fs, otherHandle, "data", 0)

	counted := &countedMetadata{Meta: f.metadata}
	f.fs.metadata = counted
	generation := f.fs.directoryGeneration()
	page, err := f.fs.directoryPage(t.Context(), dir.Object.Inode, 0, 3)
	if err != nil || len(page) != 3 {
		t.Fatalf("SQL page = %d, %v", len(page), err)
	}
	if err = f.fs.Flush(t.Context(), otherHandle, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	for _, entry := range page {
		a, attrErr := f.fs.directoryAttr(t.Context(), entry, generation)
		if attrErr != nil {
			t.Fatal(attrErr)
		}
		if entry.inode == first.Object.Inode && (a.Size != want.Size || !a.Modified.Equal(want.Modified)) {
			t.Fatalf("flushed entry = %+v, want %+v", a, want)
		}
	}
	entries, err := f.fs.ReadDir(t.Context(), dir.Object.Inode, 0, 3)
	if err != nil || len(entries) != 3 {
		t.Fatalf("page = %+v, %v", entries, err)
	}
	// ReadDir reads the directory's own attributes once.
	if counted.attrs.Load() != 1 || counted.xattrs.Load() != 0 || counted.lookups.Load() != 0 || counted.directories.Load() != 0 {
		t.Fatalf("page used whole-directory or per-entry queries: attrs=%d, xattrs=%d, lookups=%d, directories=%d",
			counted.attrs.Load(), counted.xattrs.Load(), counted.lookups.Load(), counted.directories.Load())
	}

	rows, err := f.fs.directory.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+directoryQuery, birthKey, attributesKey, accessedKey, modifiedKey, changedKey, dir.Object.Inode, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			break
		}
		indexed = indexed || strings.Contains(detail, "IDX_jfs_edge_smbfs_directory_page")
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Errorf("page sorts the directory: %s", detail)
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("page query did not use the parent and ID index")
	}
}

// A directory row read before a change must not hide the change.
func TestDirectoryRowsFollowChanges(t *testing.T) {
	initial := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	stamp := time.Unix(1000000000, 123456700).UTC()
	invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	bits := uint32(0x02)
	size := uint64(8)
	type objects struct {
		h, sh        smb.Handle
		base, stream smb.Resolved
	}
	for _, c := range []struct {
		change func(*testing.T, *fixture, objects) error
		name   string
		// Without handles nothing is retained or dirty during the change.
		handles bool
	}{
		{name: "flush", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			return f.fs.Flush(t.Context(), o.h, smb.SyncData)
		}},
		{name: "last close", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			return errors.Join(f.fs.Close(t.Context(), o.h), f.fs.Close(t.Context(), o.sh))
		}},
		{name: "close and reopen", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			err := errors.Join(f.fs.Close(t.Context(), o.h), f.fs.Close(t.Context(), o.sh))
			f.open(t, o.base.Object, smb.AccessRead)
			return err
		}},
		{name: "truncate", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			return f.fs.Truncate(t.Context(), o.h, size)
		}},
		{name: "set size", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			return f.fs.SetAttr(t.Context(), o.base.Object, smb.AttrChange{Size: &size})
		}},
		{name: "set times", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			return f.fs.SetAttr(t.Context(), o.base.Object, smb.AttrChange{Accessed: &stamp, Modified: &stamp, Changed: &stamp})
		}},
		{name: "partly failed set times", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			// Accessed is stored before Modified fails to marshal.
			err := f.fs.SetAttr(t.Context(), o.base.Object, smb.AttrChange{Accessed: &stamp, Modified: &invalid})
			if errors.Is(err, smb.ErrInvalidParameter) {
				return nil
			}
			return fmt.Errorf("set invalid time: %w", err)
		}},
		{name: "stream write", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			_, err := f.fs.WriteAt(t.Context(), o.sh, []byte("stream data"), 0)
			return err
		}},
		{name: "stream truncate", handles: true, change: func(t *testing.T, f *fixture, o objects) error {
			return f.fs.Truncate(t.Context(), o.sh, size)
		}},
		{name: "set times without handle", change: func(t *testing.T, f *fixture, o objects) error {
			return f.fs.SetAttr(t.Context(), o.base.Object, smb.AttrChange{Accessed: &stamp, Modified: &stamp, Changed: &stamp})
		}},
		{name: "set created and attributes without handle", change: func(t *testing.T, f *fixture, o objects) error {
			return f.fs.SetAttr(t.Context(), o.base.Object, smb.AttrChange{Created: &stamp, Attributes: &bits})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, 0)
			o := objects{base: f.create(t, "data", smb.KindFile), stream: f.create(t, "data:resource", smb.KindFile)}
			if err := f.fs.SetAttr(t.Context(), o.base.Object, smb.AttrChange{Created: &initial, Accessed: &initial, Modified: &initial, Changed: &initial}); err != nil {
				t.Fatal(err)
			}
			if c.handles {
				o.h = f.open(t, o.base.Object, smb.AccessWrite)
				o.sh = f.open(t, o.stream.Object, smb.AccessWrite)
				write(t, f.fs, o.h, "abcdef", 0)
			}
			generation := f.fs.directoryGeneration()
			page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 {
				t.Fatalf("page before the change = %+v, %v", page, err)
			}
			if err = c.change(t, f, o); err != nil {
				t.Fatal(err)
			}
			want, err := f.fs.GetAttr(t.Context(), o.base.Object)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.fs.directoryAttr(t.Context(), page[0], generation)
			if err != nil || !got.Created.Equal(want.Created) || !got.Accessed.Equal(want.Accessed) || !got.Modified.Equal(want.Modified) ||
				!got.Changed.Equal(want.Changed) || got.Attributes != want.Attributes || got.Size != want.Size {
				t.Fatalf("row after the change = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

type schemaColumn struct {
	sqlType             string
	notNull, primaryKey int
}

// The page query reads JuiceFS tables directly, so it depends on their schema.
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
		columns := schemaColumns(t, f.fs.directory, table.query)
		for name, expected := range table.columns {
			if columns[name] != expected {
				t.Fatalf("%s: %s = %+v, want %+v", table.query, name, columns[name], expected)
			}
		}
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

func schemaColumns(t *testing.T, db *sql.DB, query string) map[string]schemaColumn {
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
			break
		}
		columns[name] = column
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return columns
}

func requireDirectorySettings(t *testing.T, db *sql.DB) {
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

// Pages use one long-lived connection, and a replacement connection gets the
// same durability settings.
func TestDirectoryConnectionSettingsAndReuse(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "read-only"}[readOnly], func(t *testing.T) { checkDirectoryConnection(t, readOnly) })
	}
}

func checkDirectoryConnection(t *testing.T, readOnly bool) {
	t.Helper()
	f := newFixture(t, 0)
	for _, name := range []string{"first", "hidden", "last"} {
		f.create(t, name, smb.KindFile)
	}
	adapter := f.fs
	if readOnly {
		adapter = f.readOnly(t)
	}
	requireDirectorySettings(t, adapter.directory)
	// The view is connection-local. A page from a new connection would show "hidden".
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
	if err = conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("retire connection: %v", err)
	}
	if err = conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	requireDirectorySettings(t, adapter.directory)
	page, err := adapter.ReadDir(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 3 {
		t.Fatalf("replacement connection lost entries or kept the temp view: %+v, %v", page, err)
	}
}

func requireDirectoryIndex(t *testing.T, db *sql.DB) {
	t.Helper()
	var columns string
	if err := db.QueryRowContext(t.Context(), `SELECT group_concat(name,',') FROM pragma_index_info('IDX_jfs_edge_smbfs_directory_page')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != "parent,id" {
		t.Fatalf("directory index = %s", columns)
	}
}

// Metadata backups are SQLite VACUUM INTO snapshots. A database recovered from
// one keeps the page index.
func TestDirectoryIndexSurvivesSQLiteSnapshotRecovery(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "band", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "abcdef", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	// Repeating startup preparation is harmless.
	db, err := prepareDirectoryPages(f.path, false)
	if err != nil {
		t.Fatal(err)
	}
	requireDirectoryIndex(t, db)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	restoredDir := t.TempDir()
	if _, err = f.fs.directory.ExecContext(t.Context(), "VACUUM INTO ?", filepath.Join(restoredDir, "meta.db")); err != nil {
		t.Fatal(err)
	}
	recovered := fixtureAt(t, restoredDir, 0, false, 0)
	requireDirectoryIndex(t, recovered.fs.directory)
	page, err := recovered.fs.ReadDir(t.Context(), 1, 0, 10)
	if err != nil || len(page) != 1 || page[0].Name != "band" || page[0].Attr.Inode != r.Object.Inode || page[0].Attr.Size != 6 {
		t.Fatalf("recovered page = %+v, %v", page, err)
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
