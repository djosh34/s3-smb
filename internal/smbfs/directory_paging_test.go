package smbfs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

func TestDirectoryCookieSurvivesRemoval(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	a := f.create(t, "dir/a", smb.KindFile)
	f.create(t, "dir/b", smb.KindFile)
	f.create(t, "dir/c", smb.KindFile)
	f.create(t, "dir/d", smb.KindFile)
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
}

func TestDirectoryPageAvoidsPerEntryQueriesDuringRead(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	for i := range 16 {
		f.create(t, fmt.Sprintf("dir/band-%02d", i), smb.KindFile)
	}
	other := f.create(t, "other", smb.KindFile)
	h := f.open(t, other.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "data", 0)
	counted := &countedMetadata{Meta: f.metadata}
	f.fs.metadata = counted
	var reads int
	conn, err := f.fs.directory.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(raw any) error {
		sqliteConn, ok := raw.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("unexpected directory driver")
		}
		return sqliteConn.RegisterFunc("read_during_page", func() (int, error) {
			done := make(chan error, 1)
			go func() {
				data := make([]byte, 4)
				n, readErr := f.fs.ReadAt(t.Context(), h, data, 0)
				if readErr == nil && (n != 4 || string(data) != "data") {
					readErr = errors.New("unexpected concurrent read result")
				}
				done <- readErr
			}()
			reads++
			return 1, <-done
		}, false)
	})
	if err = errors.Join(err, conn.Close()); err != nil {
		t.Fatal(err)
	}
	// The connection-local view guarantees a READ overlaps the SQL page, without
	// sleeps or timing-dependent loops. It leaves the persistent schema unchanged.
	if _, err = f.fs.directory.ExecContext(t.Context(), `CREATE TEMP VIEW jfs_edge AS SELECT * FROM main.jfs_edge WHERE read_during_page()=1`); err != nil {
		t.Fatal(err)
	}
	page, err := f.fs.ReadDir(t.Context(), dir.Object.Inode, 0, 16)
	if err != nil || len(page) != 16 || reads == 0 {
		t.Fatalf("page during READ = %d entries, %d reads, %v", len(page), reads, err)
	}
	if counted.attrs.Load() != 1 || counted.xattrs.Load() != 0 || counted.lookups.Load() != 0 || counted.directories.Load() != 0 {
		t.Fatalf("READ caused per-entry queries: attrs=%d, xattrs=%d, lookups=%d, directories=%d", counted.attrs.Load(), counted.xattrs.Load(), counted.lookups.Load(), counted.directories.Load())
	}
}

func TestDirectoryPagesUseBoundedIndexedQueries(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	for i := 0; i < 256; i++ {
		f.create(t, fmt.Sprintf("dir/band-%04d", i), smb.KindFile)
	}
	counted := &countedMetadata{Meta: f.metadata}
	f.fs.metadata = counted
	page, err := f.fs.ReadDir(t.Context(), dir.Object.Inode, 0, 3)
	if err != nil || len(page) != 3 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if counted.directories.Load() != 0 || counted.lookups.Load() != 0 || counted.xattrs.Load() != 0 || counted.attrs.Load() != 1 {
		t.Fatalf("page used whole-directory or per-entry queries: %+v", counted)
	}
	raw, err := f.fs.directoryPage(t.Context(), dir.Object.Inode, page[2].Next, 3)
	if err != nil || len(raw) != 3 {
		t.Fatalf("SQL page = %d, %v", len(raw), err)
	}
	db, err := directoryDB(f.path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+directoryQuery, birthKey, attributesKey, accessedKey, modifiedKey, changedKey, dir.Object.Inode, 0, 3)
	if err != nil {
		t.Fatal(errors.Join(err, db.Close()))
	}
	t.Cleanup(func() {
		if closeErr := rows.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "IDX_jfs_edge_smbfs_directory_page") {
			indexed = true
		}
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Errorf("page sorts directory: %s", detail)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close(), db.Close()); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("page query did not use the parent/ID index")
	}
}

func TestReadDirWithConcurrentRemoval(t *testing.T) {
	f := newFixture(t, 0)
	dir := f.create(t, "dir", smb.KindDirectory)
	var removed []smb.Resolved
	for i := 0; i < 80; i++ {
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
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 80; i += 2 {
		name := fmt.Sprintf("band-%02d", i)
		if !seen[name] {
			t.Errorf("skipped live entry %s", name)
		}
	}
}

func TestDirectoryLengthAcrossFlushAndClose(t *testing.T) {
	for _, closeRef := range []bool{false, true} {
		t.Run(map[bool]string{false: "flush", true: "close"}[closeRef], func(t *testing.T) {
			f := newFixture(t, 0)
			r := f.create(t, "data", smb.KindFile)
			h := f.open(t, r.Object, smb.AccessWrite)
			write(t, f.fs, h, "abcdef", 0)
			generation := f.fs.commits.Load()
			page, err := f.fs.directoryPage(t.Context(), 1, 0, 10)
			if err != nil || len(page) != 1 || page[0].attr.Length != 0 {
				t.Fatalf("pre-commit SQL snapshot = %+v, %v", page, err)
			}
			if closeRef {
				err = f.fs.Close(t.Context(), h)
			} else {
				err = f.fs.Flush(t.Context(), h, smb.SyncData)
			}
			if err != nil {
				t.Fatal(err)
			}
			attr, err := f.fs.directoryAttr(t.Context(), page[0], generation)
			if err != nil || attr.Size != 6 {
				t.Fatalf("page across commit = %+v, %v", attr, err)
			}
		})
	}
}
