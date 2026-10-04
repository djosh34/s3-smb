package smbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"

	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestCloseCanceledContextStillReleases(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "data", 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireError(t, f.fs.Close(ctx, h), context.Canceled)
	requireError(t, f.fs.Close(t.Context(), h), smb.ErrInvalidHandle)
	if len(f.fs.inodes) != 0 {
		t.Fatal("canceled close retained native reference")
	}
}

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

func TestNonDataAttributeChangesDoNotUpload(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessWrite)
	write(t, f.fs, h, "dirty", 0)
	puts := f.store.puts.Load()
	stamp := time.Unix(1000000000, 0)
	bits := uint32(2)
	if err := f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Created: &stamp, Attributes: &bits}); err != nil {
		t.Fatal(err)
	}
	if f.store.puts.Load() != puts {
		t.Fatal("non-data attribute change uploaded buffered data")
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != 5 || !a.Created.Equal(stamp) || a.Attributes != bits {
		t.Fatalf("attr = %+v, %v", a, err)
	}
}

func TestDirectoryRenameRejectsDescendant(t *testing.T) {
	f := newFixture(t, 0)
	source := f.create(t, "a", smb.KindDirectory)
	f.create(t, "a/b", smb.KindDirectory)
	descendant := f.create(t, "a/b/c", smb.KindDirectory)
	err := f.fs.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: smb.Name{Parent: descendant.Object.Inode, Base: "moved"}, SourceInode: source.Object.Inode})
	if !errors.Is(err, smb.ErrInvalidParameter) {
		t.Fatalf("descendant move = %v", err)
	}
	r, err := f.fs.Lookup(t.Context(), "a/b/c")
	if err != nil || !r.Exists {
		t.Fatalf("rejected move changed subtree: %+v, %v", r, err)
	}
	destination := f.create(t, "destination", smb.KindDirectory)
	if err = f.fs.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: smb.Name{Parent: destination.Object.Inode, Base: "moved"}, SourceInode: source.Object.Inode}); err != nil {
		t.Fatal(err)
	}
	r, err = f.fs.Lookup(t.Context(), "destination/moved/b/c")
	if err != nil || !r.Exists || r.Object.Inode != descendant.Object.Inode {
		t.Fatalf("ordinary move lost subtree: %+v, %v", r, err)
	}
}

func TestBackendFileSizeBoundary(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "abc", 0)
	puts := f.store.puts.Load()
	for _, size := range []uint64{maxFileSize, maxFileSize + 1, math.MaxUint64} {
		requireError(t, f.fs.Truncate(t.Context(), h, size), smb.ErrFileTooLarge)
		requireError(t, f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}), smb.ErrFileTooLarge)
		_, err := f.fs.WriteAt(t.Context(), h, []byte("x"), size-1)
		requireError(t, err, smb.ErrFileTooLarge)
		_, err = f.fs.ReadAt(t.Context(), h, make([]byte, 1), size-1)
		requireError(t, err, io.EOF)
	}
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != 3 || f.store.puts.Load() != puts {
		t.Fatalf("invalid range mutated data: %+v, %v, puts=%d", a, err, f.store.puts.Load())
	}
	read(t, f.fs, h, []byte("abc"))
	if err = f.fs.Truncate(t.Context(), h, maxFileSize-1); err != nil {
		t.Fatal(err)
	}
	write(t, f.fs, h, "z", maxFileSize-2)
	data := make([]byte, 1)
	n, err := f.fs.ReadAt(t.Context(), h, data, maxFileSize-2)
	if err != nil || n != 1 || data[0] != 'z' {
		t.Fatalf("last supported byte = %q, %d, %v", data, n, err)
	}
	if err = f.fs.Truncate(t.Context(), h, 3); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte("abc"))
}

func TestRetainedReferencesWorkWithTrash(t *testing.T) {
	for _, days := range []int{0, 14} {
		t.Run(strconv.Itoa(days), func(t *testing.T) { testRetainedReferences(t, days) })
	}
}

func testRetainedReferences(t *testing.T, days int) {
	t.Helper()
	f := fixtureAt(t, t.TempDir(), 0, true, days)
	r := f.create(t, "data", smb.KindFile)
	stream := f.create(t, "data:fork", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	sh := f.open(t, stream.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "base", 0)
	write(t, f.fs, sh, "stream", 0)
	if err := f.fs.Remove(t.Context(), r.Name, r.Object.Inode); err != nil {
		t.Fatal(err)
	}
	_, err := f.fs.PathOf(t.Context(), r.Object.Inode)
	requireError(t, err, smb.ErrNameNotFound)
	read(t, f.fs, h, []byte("base"))
	read(t, f.fs, sh, []byte("stream"))
	write(t, f.fs, h, "new", 0)
	write(t, f.fs, sh, "new", 0)
	if err = f.fs.Truncate(t.Context(), h, 3); err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Truncate(t.Context(), sh, 3); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1000000000, 123456700)
	bits := uint32(0x21)
	if err = f.fs.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp, Created: &stamp, Attributes: &bits}); err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	read(t, f.fs, h, []byte("new"))
	read(t, f.fs, sh, []byte("new"))
	a, err := f.fs.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Modified.Equal(stamp) {
		t.Fatalf("retained attrs = %+v, %v", a, err)
	}
	if _, err = f.fs.Lookup(t.Context(), ".trash"); err == nil {
		t.Fatal("trash admitted into namespace")
	}
	if err = f.fs.Close(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if err = f.fs.Close(t.Context(), sh); err != nil {
		t.Fatal(err)
	}
	if days == 0 {
		assertDiscardedInode(t, f, r.Object.Inode)
	}
}

func assertDiscardedInode(t *testing.T, f *fixture, ino smb.Inode) {
	t.Helper()
	for _, query := range []string{"SELECT count(*) FROM jfs_node WHERE inode=?", "SELECT count(*) FROM jfs_xattr WHERE inode=?"} {
		var count int
		if err := f.fs.directory.QueryRowContext(t.Context(), query, ino).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("last close left %d rows: %s", count, query)
		}
	}
}

func TestMetadataQueriesDoNotWaitForColdRead(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "cold", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	f.store.cold.Store(true)
	readDone := make(chan error, 1)
	go func() { _, err := f.fs.ReadAt(t.Context(), h, make([]byte, 4), 0); readDone <- err }()
	select {
	case <-f.store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("cold GET not reached")
	}
	queriesDone := make(chan error, 1)
	go func() {
		_, err := f.fs.GetAttr(t.Context(), r.Object)
		if err == nil {
			_, err = f.fs.Lookup(t.Context(), "data")
		}
		if err == nil {
			_, err = f.fs.ReadDir(t.Context(), 1, 0, 10)
		}
		queriesDone <- err
	}()
	select {
	case err := <-queriesDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("metadata query waited for cold read")
	}
	completed := false
	select {
	case err := <-readDone:
		completed = true
		t.Errorf("cold read completed before release: %v", err)
	default:
	}
	f.store.cold.Store(false)
	close(f.store.resume)
	if !completed {
		if err := <-readDone; err != nil {
			t.Fatal(err)
		}
	}
}

type countedMetadata struct {
	meta.Meta
	attrs       atomic.Int64
	xattrs      atomic.Int64
	lookups     atomic.Int64
	directories atomic.Int64
	denyLoad    bool
}

func (m *countedMetadata) GetAttr(ctx meta.Context, ino meta.Ino, a *meta.Attr) syscall.Errno {
	m.attrs.Add(1)
	return m.Meta.GetAttr(ctx, ino, a)
}

func (m *countedMetadata) GetXattr(ctx meta.Context, ino meta.Ino, key string, value *[]byte) syscall.Errno {
	m.xattrs.Add(1)
	return m.Meta.GetXattr(ctx, ino, key, value)
}

func (m *countedMetadata) Lookup(ctx meta.Context, parent meta.Ino, name string, ino *meta.Ino, a *meta.Attr, check bool) syscall.Errno {
	m.lookups.Add(1)
	return m.Meta.Lookup(ctx, parent, name, ino, a, check)
}

func (m *countedMetadata) Readdir(ctx meta.Context, ino meta.Ino, plus uint8, entries *[]*meta.Entry) syscall.Errno {
	m.directories.Add(1)
	return m.Meta.Readdir(ctx, ino, plus, entries)
}

func (m *countedMetadata) Load(check bool) (*meta.Format, error) {
	if m.denyLoad {
		return nil, errors.New("unexpected format reload")
	}
	return m.Meta.Load(check)
}

func TestIOUsesRetainedKindAndLength(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	counted := &countedMetadata{Meta: f.metadata}
	f.fs.metadata = counted
	for i := 0; i < 5; i++ {
		write(t, f.fs, h, "data", 0)
		read(t, f.fs, h, []byte("data"))
	}
	if counted.attrs.Load() != 0 || counted.xattrs.Load() != 0 {
		t.Fatalf("adapter queried attributes during I/O: attr=%d, xattr=%d", counted.attrs.Load(), counted.xattrs.Load())
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

func TestConstructorUsesCurrentFormat(t *testing.T) {
	f := newFixture(t, 0)
	counted := &countedMetadata{Meta: f.metadata}
	filesystem, err := jfs.NewFileSystem(f.config, counted, f.chunks, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted.denyLoad = true
	adapter, err := New(Options{Filesystem: filesystem, Barrier: f.fs.barrier, Config: f.config, Store: f.chunks, MetadataPath: f.path})
	if err != nil {
		t.Error(err)
	} else if err = adapter.Shutdown(); err != nil {
		t.Error(err)
	}
	if err = filesystem.Close(); err != nil {
		t.Fatal(err)
	}
}
