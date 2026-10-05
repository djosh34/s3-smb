// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

type memObject struct {
	modified time.Time
	data     []byte
}

// memBucket is an S3 bucket in memory. fault runs before a request and an
// error fails it with no effect. landed runs after a request took effect,
// and an error loses its response.
type memBucket struct {
	objects  map[string]memObject
	fault    func(op, key string) error
	landed   func(op, key string) error
	requests []string
	mu       sync.Mutex
}

func newMemBucket() *memBucket { return &memBucket{objects: make(map[string]memObject)} }

func (b *memBucket) hooks() (fault, landed func(op, key string) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fault, b.landed
}

func (b *memBucket) setFault(fault func(op, key string) error) {
	b.mu.Lock()
	b.fault = fault
	b.mu.Unlock()
}

func (b *memBucket) setLanded(landed func(op, key string) error) {
	b.mu.Lock()
	b.landed = landed
	b.mu.Unlock()
}

// do runs one request: the fault hook, apply, then the landed hook.
func (b *memBucket) do(ctx context.Context, op, key string, apply func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fault, landed := b.hooks()
	if fault != nil {
		if err := fault(op, key); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.requests = append(b.requests, op+" "+key)
	apply()
	b.mu.Unlock()
	if landed != nil {
		return landed(op, key)
	}
	return nil
}

func (b *memBucket) put(ctx context.Context, key string, data []byte) error {
	data = bytes.Clone(data)
	return b.do(ctx, "put", key, func() { b.objects[key] = memObject{data: data, modified: time.Now()} })
}

func (b *memBucket) get(ctx context.Context, key string, offset, length uint64) ([]byte, error) {
	var data []byte
	var err error
	doErr := b.do(ctx, "get", key, func() {
		object, ok := b.objects[key]
		switch {
		case !ok:
			err = fmt.Errorf("get %s: %w", key, errNotFound)
		case length == 0:
			data = bytes.Clone(object.data)
		case offset+length > uint64(len(object.data)):
			err = fmt.Errorf("get %s: range %d+%d past %d bytes", key, offset, length, len(object.data))
		default:
			data = bytes.Clone(object.data[offset : offset+length])
		}
	})
	return data, errors.Join(doErr, err)
}

func (b *memBucket) remove(ctx context.Context, key string) error {
	return b.do(ctx, "delete", key, func() { delete(b.objects, key) })
}

func (b *memBucket) list(ctx context.Context, prefix string) ([]object, error) {
	var result []object
	err := b.do(ctx, "list", prefix, func() {
		for key, o := range b.objects {
			if strings.HasPrefix(key, prefix) {
				result = append(result, object{key: key, size: int64(len(o.data)), modified: o.modified})
			}
		}
	})
	slices.SortFunc(result, func(a, b object) int { return strings.Compare(a.key, b.key) })
	return result, err
}

func (b *memBucket) keys(prefix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keys []string
	for key := range b.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func (b *memBucket) data(key string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[key]
	return o.data, ok
}

// clone copies the bucket, as a second view for a test that rolls back.
func (b *memBucket) clone() *memBucket {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := newMemBucket()
	for key, o := range b.objects {
		c.objects[key] = memObject{data: bytes.Clone(o.data), modified: o.modified}
	}
	return c
}

// testTuning uses 16-byte chunks and a 4-chunk RAM budget, so small writes
// cross chunks and upload early. Copies run only when a test asks, and the
// lease outlives any test unless a test shortens it. Another server's lock
// is stale at once, so a new data folder need not wait.
func testTuning() tuning {
	t := defaultTuning()
	t.chunkSize = 16
	t.dirtyChunks = 4
	t.copyInterval = 24 * time.Hour
	t.stopAge = 24 * time.Hour
	t.renewEvery = time.Hour
	t.lease = 24 * time.Hour
	t.stale = 0
	t.lockRetry = 20 * time.Millisecond
	t.copyRetry = 5 * time.Millisecond
	t.watchEvery = 5 * time.Millisecond
	return t
}

type fixture struct {
	t      testing.TB
	bucket *memBucket
	dir    string
	tune   tuning
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	return &fixture{t: t, bucket: newMemBucket(), dir: t.TempDir(), tune: testTuning()}
}

// open starts an engine. Cleanup shuts it down unless the test did.
func (f *fixture) open() *Engine {
	f.t.Helper()
	e, err := f.tryOpen()
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *fixture) tryOpen() (*Engine, error) {
	e, err := open(context.Background(), Options{Dir: f.dir}, f.bucket, f.tune)
	if err != nil {
		return nil, err
	}
	f.t.Cleanup(func() { kill(f.t, e) })
	return e, nil
}

// kill stops an engine as a crash would: no flush and no clean exit.
func kill(t testing.TB, e *Engine) {
	t.Helper()
	e.fail(errors.New("killed by the test"))
	e.group.Wait()
	if err := e.db.Close(); err != nil {
		t.Error(err)
	}
}

// shutdown stops an engine cleanly.
func shutdown(t testing.TB, e *Engine) {
	t.Helper()
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// snapshot copies the data folder as a power loss would leave it: every
// committed transaction, including those still in the WAL.
func snapshot(t testing.TB, e *Engine) string {
	t.Helper()
	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	dir := t.TempDir()
	for _, name := range []string{databaseName, databaseName + "-wal", serverIDName} {
		data, err := os.ReadFile(filepath.Join(e.dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func requireError(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// create makes a file or directory at path, whose parent must exist.
func create(t testing.TB, e *Engine, path string, kind smb.Kind) smb.Resolved {
	t.Helper()
	r, err := e.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := e.Create(t.Context(), r.Name, kind)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func lookup(t testing.TB, e *Engine, path string) smb.Resolved {
	t.Helper()
	r, err := e.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Exists {
		t.Fatalf("%s does not exist", path)
	}
	return r
}

func openFile(t testing.TB, e *Engine, path string, access smb.Access) smb.Handle {
	t.Helper()
	h, err := e.Open(t.Context(), lookup(t, e, path).Object, access)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func closeFile(t testing.TB, e *Engine, h smb.Handle) {
	t.Helper()
	if err := e.Close(t.Context(), h); err != nil {
		t.Fatal(err)
	}
}

func writeAt(t testing.TB, e *Engine, h smb.Handle, data string, offset uint64) {
	t.Helper()
	n, err := e.WriteAt(t.Context(), h, []byte(data), offset)
	if err != nil || n != len(data) {
		t.Fatalf("write = %d, %v", n, err)
	}
}

func flush(t testing.TB, e *Engine, h smb.Handle) {
	t.Helper()
	if err := e.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}
}

// writeFile creates or replaces path's content and flushes it.
func writeFile(t testing.TB, e *Engine, path, data string) {
	t.Helper()
	r, err := e.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Exists {
		create(t, e, path, smb.KindFile)
	}
	h := openFile(t, e, path, smb.AccessRead|smb.AccessWrite)
	if err = e.Truncate(t.Context(), h, 0); err != nil {
		t.Fatal(err)
	}
	writeAt(t, e, h, data, 0)
	flush(t, e, h)
	closeFile(t, e, h)
}

// readHandle reads everything through h.
func readHandle(t testing.TB, e *Engine, h smb.Handle) string {
	t.Helper()
	var out []byte
	buffer := make([]byte, 37)
	for {
		n, err := e.ReadAt(t.Context(), h, buffer, uint64(len(out)))
		out = append(out, buffer[:n]...)
		if errors.Is(err, io.EOF) {
			return string(out)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t testing.TB, e *Engine, path string) string {
	t.Helper()
	h := openFile(t, e, path, smb.AccessRead)
	defer closeFile(t, e, h)
	return readHandle(t, e, h)
}

// tree reads every file and directory under the root. Directories end in /.
func tree(t testing.TB, e *Engine) map[string]string {
	t.Helper()
	out := make(map[string]string)
	var walk func(path string, ino smb.Inode)
	walk = func(path string, ino smb.Inode) {
		entries, err := e.ReadDir(t.Context(), ino, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			child := path + entry.Name
			if entry.Attr.Kind == smb.KindDirectory {
				out[child+"/"] = ""
				walk(child+"/", entry.Attr.Inode)
				continue
			}
			out[child] = readFile(t, e, child)
		}
	}
	walk("", rootInode)
	return out
}

// checkCopies opens every copy in the bucket and requires each chunk it
// names to exist, so each one can still be restored.
func checkCopies(t testing.TB, b *memBucket) {
	t.Helper()
	for _, key := range b.keys(copyPrefix) {
		data, _ := b.data(key)
		path := filepath.Join(t.TempDir(), "copy")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		db := openSQLite(path, "ro")
		names, err := chunkNames(t.Context(), db)
		if err = errors.Join(err, db.Close()); err != nil {
			t.Fatal(key, err)
		}
		for _, name := range names {
			if _, ok := b.data(chunkPrefix + name); !ok {
				t.Fatalf("copy %s names chunk %s, which is gone", key, name)
			}
		}
	}
}

func chunkNames(ctx context.Context, db *sql.DB) (names []string, err error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func copyNow(t testing.TB, e *Engine) {
	t.Helper()
	if err := e.makeCopy(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
}
