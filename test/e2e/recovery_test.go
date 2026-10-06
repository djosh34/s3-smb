// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestRecovery recovers twice on a new data folder from the database copies
// in the bucket.
func TestRecovery(t *testing.T) {
	f := newFixture(t)
	d := f.start()
	share, disconnect := f.share()
	large := make([]byte, 17_000_003)
	for i := range large {
		large[i] = byte((i*31 + i/257) % 251)
	}
	files := map[string][]byte{"empty": {}, "unicode-文件.txt": []byte("all expected files are checked\n"), "large.bin": large}
	for name, data := range files {
		writeFile(t, share, name, data)
	}
	verifyFiles(t, share, files)
	disconnect()
	f.copyDatabase(d)
	recoverTwice(t, f, files)
}

// recoverTwice starts on a new data folder, which restores the newest copy,
// checks the files and writes a new one. Then it makes a copy that holds the
// new file and does the same once more. No daemon may run, and the newest copy
// must hold files.
func recoverTwice(t *testing.T, f *fixture, files map[string][]byte) {
	t.Helper()
	d := recoverFresh(t, f, files)
	share, disconnect := f.share()
	files["after-recovery.txt"] = []byte("written on top of the recovered state\n")
	writeFile(t, share, "after-recovery.txt", files["after-recovery.txt"])
	disconnect()
	f.copyDatabase(d)
	recoverFresh(t, f, files).stop()
}

// recoverFresh starts on a new data folder, checks that the start restored
// the newest copy and that the share holds files, and returns the daemon.
func recoverFresh(t *testing.T, f *fixture, files map[string][]byte) *daemon {
	t.Helper()
	f.freshLocal()
	newest := f.newestCopy()
	d := f.start()
	if restored := d.restoredCopy(); restored != newest {
		t.Fatalf("a new data folder restored copy %q, want the newest %q", restored, newest)
	}
	share, disconnect := f.share()
	verifyFiles(t, share, files)
	disconnect()
	return d
}

// TestRecoveryWithoutServer reads a file back from the bucket without s3-smb,
// the way docs/recovery.md describes. Chunks 0 and 2 are stored, chunk 1 is a
// hole, and the file ends in zeros after chunk 2.
func TestRecoveryWithoutServer(t *testing.T) {
	const name = "Mac.sparsebundle/bands/1"
	want := make([]byte, 3*chunkSize+3<<20)
	for i := range chunkSize {
		want[i] = byte((i*7 + i/97) % 253)
	}
	tail := want[2*chunkSize+100 : 2*chunkSize+100+1<<20]
	for i := range tail {
		tail[i] = byte(i%241 + 1)
	}
	f := newFixture(t)
	d := f.start()
	share, disconnect := f.share()
	if err := share.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	file, err := share.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt(want[:chunkSize], 0); err == nil {
		_, err = file.WriteAt(tail, 2*chunkSize+100)
	}
	if err == nil {
		err = file.Truncate(int64(len(want)))
	}
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		t.Fatal(err)
	}
	verifyFiles(t, share, map[string][]byte{name: want})
	disconnect()
	f.copyDatabase(d)

	got, indexes := readWithoutServer(t, f, name)
	if !slices.Equal(indexes, []int64{0, 2}) {
		t.Fatalf("the file has chunks %v, want 0 and 2 around a hole", indexes)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("SHA256 mismatch: got %x want %x", sha256.Sum256(got), sha256.Sum256(want))
	}
}

// chunkSize is the chunk size of docs/recovery.md.
const chunkSize = 8388608

// The query of docs/recovery.md, with the path as a parameter.
const chunksQuery = `WITH RECURSIVE path(id, name) AS (
  SELECT id, name FROM files WHERE parent = 1
  UNION ALL
  SELECT files.id, path.name || '/' || files.name FROM files JOIN path ON files.parent = path.id
)
SELECT chunks.idx, chunks.name, chunks.length
FROM path JOIN chunks ON chunks.file = path.id
WHERE path.name = ?
ORDER BY chunks.idx;`

// sizeQuery reads the file's size from the files table.
const sizeQuery = `WITH RECURSIVE path(id, name) AS (
  SELECT id, name FROM files WHERE parent = 1
  UNION ALL
  SELECT files.id, path.name || '/' || files.name FROM files JOIN path ON files.parent = path.id
)
SELECT files.size FROM path JOIN files ON files.id = path.id WHERE path.name = ?;`

// readWithoutServer downloads the newest copy, lists the file's chunks with
// the documented query and puts the first length bytes of chunk idx at
// idx * 8388608 in a file of the stored size. It returns the file and its
// chunk indexes.
func readWithoutServer(t *testing.T, f *fixture, name string) ([]byte, []int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, f.object(f.newestCopy()), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, db)
	var size int64
	if err = db.QueryRowContext(t.Context(), sizeQuery, name).Scan(&size); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(t.Context(), chunksQuery, name)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, size)
	var indexes []int64
	for rows.Next() {
		var idx, length int64
		var chunk string
		if err = rows.Scan(&idx, &chunk, &length); err != nil {
			break
		}
		object := f.object("chunks/" + chunk)
		if int64(len(object)) < length || idx*chunkSize+length > size {
			t.Fatalf("chunk %d (%s) of %d bytes does not fit: object %d bytes, file %d bytes", idx, chunk, length, len(object), size)
		}
		copy(data[idx*chunkSize:], object[:length])
		indexes = append(indexes, idx)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return data, indexes
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	d := f.start()
	share, disconnect := f.share()
	writeFile(t, share, "signed.txt", []byte("signed session"))
	disconnect()
	for _, credentials := range [][2]string{{"backup", "wrong-password"}, {"wrong-user", f.password}} {
		if _, disconnect, err := f.connect(credentials[0], credentials[1]); err == nil {
			disconnect()
			t.Errorf("user %q with password %q authenticated", credentials[0], credentials[1])
		}
	}
	d.stop()
}

// TestReadOnly restarts a written volume read-only and checks that it serves
// the files and refuses changes.
func TestReadOnly(t *testing.T) {
	f := newFixture(t)
	d := f.start()
	share, disconnect := f.share()
	data := []byte("read-only fixture")
	writeFile(t, share, "keep.txt", data)
	disconnect()
	d.stop()
	f.readonly = true
	d = f.start()
	share, disconnect = f.share()
	verifyFiles(t, share, map[string][]byte{"keep.txt": data})
	if file, err := share.Create("forbidden.txt"); err == nil {
		t.Error(errors.Join(errors.New("read-only CREATE succeeded"), file.Close()))
	}
	if err := share.Remove("keep.txt"); err == nil {
		t.Error("read-only DELETE succeeded")
	}
	disconnect()
	d.stop()
}
