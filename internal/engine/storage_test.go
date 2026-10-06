// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

var errChunkFault = errors.New("chunk request failed")

// storageGate holds an operation at one point until the test opens it.
type storageGate struct {
	entered chan struct{}
	resume  chan struct{}
	enter   sync.Once
	leave   sync.Once
}

func newStorageGate(t *testing.T) *storageGate {
	g := &storageGate{entered: make(chan struct{}), resume: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

// pause marks the gate entered and waits until it opens.
func (g *storageGate) pause() {
	g.enter.Do(func() { close(g.entered) })
	<-g.resume
}

func (g *storageGate) open() { g.leave.Do(func() { close(g.resume) }) }

// holdChunks makes every chunk request of kind op wait at the gate.
func holdChunks(f *fixture, op string, g *storageGate) {
	f.bucket.setFault(func(o, key string) error {
		if o == op && strings.HasPrefix(key, chunkPrefix) {
			g.pause()
		}
		return nil
	})
}

// failChunkPuts fails every chunk upload and counts the attempts.
func failChunkPuts(f *fixture) *atomic.Int64 {
	attempts := new(atomic.Int64)
	f.bucket.setFault(func(op, key string) error {
		if op == "put" && strings.HasPrefix(key, chunkPrefix) {
			attempts.Add(1)
			return errChunkFault
		}
		return nil
	})
	return attempts
}

// chunkRequests counts the chunk requests the bucket has served.
func chunkRequests(b *memBucket) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, request := range b.requests {
		if strings.Contains(request, " "+chunkPrefix) {
			n++
		}
	}
	return n
}

// storageOpen starts an engine on the fixture's folder and bucket. Cleanup
// stops it as a crash would.
func storageOpen(t *testing.T, f *fixture, options Options) *Engine {
	t.Helper()
	options.Dir = f.dir
	e, err := open(context.Background(), options, f.bucket, f.tune)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kill(t, e) })
	return e
}

func storageCount(t *testing.T, e *Engine, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func storageInodes(e *Engine) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inodes)
}

func requireContent(t *testing.T, e *Engine, h smb.Handle, want string) {
	t.Helper()
	if got := readHandle(t, e, h); got != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

// storageReceive returns the next value from ch, and fails the test instead
// of hanging when an operation blocks.
func storageReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
	}
	t.Fatal("operation blocked")
	var zero T
	return zero
}

func zeros(n int) string { return strings.Repeat("\x00", n) }

func TestStorageLookupPaths(t *testing.T) {
	e := newFixture(t).open()
	root, err := e.Lookup(t.Context(), "")
	if err != nil || !root.Exists || root.Object != rootInode || root.Attr.Kind != smb.KindDirectory {
		t.Fatalf("root = %+v, %v", root, err)
	}
	dir := create(t, e, "dir", smb.KindDirectory)
	file := create(t, e, "dir/a", smb.KindFile)
	nested, err := e.Lookup(t.Context(), "dir\\a::$DATA")
	if err != nil || !nested.Exists || nested.Object != file.Object {
		t.Fatalf("nested = %+v, %v", nested, err)
	}
	for _, p := range []string{"../data", "dir/../data", "/data", "dir//a", "dir/.", "dir/a\x00", "dir/a:bad:other", "dir/a:", "dir/" + strings.Repeat("x", 256)} {
		if _, err = e.Lookup(t.Context(), p); err == nil {
			t.Errorf("accepted path %q", p)
		}
	}
	_, err = e.Lookup(t.Context(), "absent/leaf")
	requireError(t, err, smb.ErrPathNotFound)
	_, err = e.Lookup(t.Context(), "dir/a/leaf")
	requireError(t, err, smb.ErrNotDirectory)
	missing, err := e.Lookup(t.Context(), "dir/missing")
	if err != nil || missing.Exists || missing.Name.Parent != dir.Object || missing.Name.Base != "missing" {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
	_, err = e.Open(t.Context(), 0, smb.AccessRead)
	requireError(t, err, smb.ErrInvalidParameter)
}

func TestStorageCreateIsExclusive(t *testing.T) {
	e := newFixture(t).open()
	missing, err := e.Lookup(t.Context(), "data")
	if err != nil || missing.Exists {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
	created, err := e.Create(t.Context(), missing.Name, smb.KindFile)
	if err != nil || !created.Exists || created.Attr.Kind != smb.KindFile || created.Attr.Inode != created.Object {
		t.Fatalf("created = %+v, %v", created, err)
	}
	h, err := e.Open(t.Context(), created.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	requireContent(t, e, h, "")
	writeAt(t, e, h, "created", 0)
	for _, kind := range []smb.Kind{smb.KindFile, smb.KindDirectory} {
		_, err = e.Create(t.Context(), missing.Name, kind)
		requireError(t, err, smb.ErrNameCollision)
	}
	requireContent(t, e, h, "created")
	closeFile(t, e, h)
	_, err = e.Create(t.Context(), smb.Name{Parent: created.Object, Base: "child"}, smb.KindFile)
	requireError(t, err, smb.ErrNotDirectory)
	_, err = e.Create(t.Context(), smb.Name{Parent: created.Object + 100, Base: "child"}, smb.KindFile)
	requireError(t, err, smb.ErrNameNotFound)
}

func TestStorageNamespaceChangesRequireExpectedIdentities(t *testing.T) {
	e := newFixture(t).open()
	source := create(t, e, "source", smb.KindFile)
	destination := create(t, e, "destination", smb.KindFile)
	a := openFile(t, e, "source", smb.AccessRead|smb.AccessWrite)
	b := openFile(t, e, "destination", smb.AccessRead|smb.AccessWrite)
	writeAt(t, e, a, "source", 0)
	writeAt(t, e, b, "destination", 0)
	requireError(t, e.Remove(t.Context(), source.Name, destination.Object), smb.ErrIdentityChanged)
	request := smb.RenameRequest{Source: source.Name, Destination: destination.Name, SourceInode: source.Object, Replace: true}
	requireError(t, e.Rename(t.Context(), request), smb.ErrIdentityChanged)
	request.DestinationInode = destination.Object
	request.SourceInode = destination.Object
	requireError(t, e.Rename(t.Context(), request), smb.ErrIdentityChanged)
	requireContent(t, e, a, "source")
	requireContent(t, e, b, "destination")
	request.SourceInode = source.Object
	if err := e.Rename(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	path, err := e.PathOf(t.Context(), source.Object)
	if err != nil || path != "destination" {
		t.Fatalf("path = %q, %v", path, err)
	}
	_, err = e.PathOf(t.Context(), destination.Object)
	requireError(t, err, smb.ErrNameNotFound)
	requireContent(t, e, a, "source")
	requireContent(t, e, b, "destination")
	if err = e.Remove(t.Context(), destination.Name, source.Object); err != nil {
		t.Fatal(err)
	}
	_, err = e.PathOf(t.Context(), source.Object)
	requireError(t, err, smb.ErrNameNotFound)
	requireContent(t, e, a, "source")
}

func TestStorageStreamSyntaxIsNotSupported(t *testing.T) {
	e := newFixture(t).open()
	writeFile(t, e, "data", "payload")
	for _, p := range []string{"data:fork", "data:fork:$DATA"} {
		_, err := e.Lookup(t.Context(), p)
		requireError(t, err, smb.ErrNotSupported)
	}
	if got := tree(t, e); !maps.Equal(got, map[string]string{"data": "payload"}) {
		t.Fatalf("tree = %v", got)
	}
}

func TestStorageDirectoryRenameRejectsDescendant(t *testing.T) {
	e := newFixture(t).open()
	source := create(t, e, "a", smb.KindDirectory)
	create(t, e, "a/b", smb.KindDirectory)
	descendant := create(t, e, "a/b/c", smb.KindDirectory)
	for _, parent := range []smb.Inode{source.Object, descendant.Object} {
		err := e.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: smb.Name{Parent: parent, Base: "moved"}, SourceInode: source.Object})
		requireError(t, err, smb.ErrInvalidParameter)
	}
	if r := lookup(t, e, "a/b/c"); r.Object != descendant.Object {
		t.Fatalf("rejected move changed the subtree: %+v", r)
	}
	destination := create(t, e, "destination", smb.KindDirectory)
	if err := e.Rename(t.Context(), smb.RenameRequest{Source: source.Name, Destination: smb.Name{Parent: destination.Object, Base: "moved"}, SourceInode: source.Object}); err != nil {
		t.Fatal(err)
	}
	if r := lookup(t, e, "destination/moved/b/c"); r.Object != descendant.Object {
		t.Fatalf("ordinary move lost the subtree: %+v", r)
	}
}

func TestStorageReadOnlyRejectsAllMutations(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "dir", smb.KindDirectory)
	writeFile(t, e, "data", "read-only payload")
	shutdown(t, e)
	ro := storageOpen(t, f, Options{ReadOnly: true})
	r := lookup(t, ro, "data")
	for _, access := range []smb.Access{smb.AccessWrite, smb.AccessAppend, smb.AccessRead | smb.AccessWrite} {
		_, err := ro.Open(t.Context(), r.Object, access)
		requireError(t, err, smb.ErrReadOnly)
	}
	h, err := ro.Open(t.Context(), r.Object, smb.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ro.WriteAt(t.Context(), h, []byte("x"), 0)
	requireError(t, err, smb.ErrReadOnly)
	requireError(t, ro.Truncate(t.Context(), h, 0), smb.ErrReadOnly)
	size, stamp := uint64(0), time.Unix(1000000000, 0)
	for _, change := range []smb.AttrChange{{Size: &size}, {Modified: &stamp}} {
		requireError(t, ro.SetAttr(t.Context(), r.Object, change), smb.ErrReadOnly)
	}
	requireError(t, ro.Remove(t.Context(), r.Name, r.Object), smb.ErrReadOnly)
	_, err = ro.Create(t.Context(), smb.Name{Parent: rootInode, Base: "new"}, smb.KindFile)
	requireError(t, err, smb.ErrReadOnly)
	requireError(t, ro.Rename(t.Context(), smb.RenameRequest{Source: r.Name, Destination: smb.Name{Parent: rootInode, Base: "new"}, SourceInode: r.Object}), smb.ErrReadOnly)
	if err = ro.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	requireContent(t, ro, h, "read-only payload")
	closeFile(t, ro, h)
	if got := tree(t, ro); !maps.Equal(got, map[string]string{"dir/": "", "data": "read-only payload"}) {
		t.Fatalf("tree = %v", got)
	}
}

// A removed file stays usable through its handles. The last close drops its
// row and sends its chunks to the trash.
func TestStorageRemovedOpenFileWorksUntilLastClose(t *testing.T) {
	e := newFixture(t).open()
	old := strings.Repeat("old!", 10)
	writeFile(t, e, "data", old)
	r := lookup(t, e, "data")
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	other := openFile(t, e, "data", smb.AccessRead)
	if err := e.Remove(t.Context(), r.Name, r.Object); err != nil {
		t.Fatal(err)
	}
	_, err := e.PathOf(t.Context(), r.Object)
	requireError(t, err, smb.ErrNameNotFound)
	if gone, lookupErr := e.Lookup(t.Context(), "data"); lookupErr != nil || gone.Exists {
		t.Fatalf("removed name = %+v, %v", gone, lookupErr)
	}
	requireContent(t, e, other, old)
	writeAt(t, e, h, "new", 0)
	if err = e.Truncate(t.Context(), h, 3); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1000000000, 123456700).UTC()
	bits := uint32(0x21)
	if err = e.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp, Created: &stamp, Attributes: &bits}); err != nil {
		t.Fatal(err)
	}
	if err = e.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	requireContent(t, e, other, "new")
	a, err := e.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != 3 || !a.Modified.Equal(stamp) || !a.Created.Equal(stamp) || a.Attributes != bits {
		t.Fatalf("retained attrs = %+v, %v", a, err)
	}
	closeFile(t, e, h)
	chunks := storageCount(t, e, `SELECT count(*) FROM chunks WHERE file = ?`, r.Object)
	trash := storageCount(t, e, `SELECT count(*) FROM trash`)
	if chunks == 0 {
		t.Fatal("the removed file has no chunks before its last close")
	}
	closeFile(t, e, other)
	if n := storageCount(t, e, `SELECT count(*) FROM files WHERE id = ?`, r.Object); n != 0 {
		t.Fatalf("last close left %d file rows", n)
	}
	if n := storageCount(t, e, `SELECT count(*) FROM chunks WHERE file = ?`, r.Object); n != 0 {
		t.Fatalf("last close left %d chunk rows", n)
	}
	if n := storageCount(t, e, `SELECT count(*) FROM trash`); n != trash+chunks {
		t.Fatalf("trash has %d rows, want %d", n, trash+chunks)
	}
}

func TestStoragePathOfFollowsRenames(t *testing.T) {
	e := newFixture(t).open()
	dir := create(t, e, "dir", smb.KindDirectory)
	file := create(t, e, "dir/file", smb.KindFile)
	for ino, want := range map[smb.Inode]string{rootInode: "", dir.Object: "dir", file.Object: "dir/file"} {
		if path, err := e.PathOf(t.Context(), ino); err != nil || path != want {
			t.Fatalf("path of %d = %q, %v; want %q", ino, path, err, want)
		}
	}
	if err := e.Rename(t.Context(), smb.RenameRequest{Source: dir.Name, Destination: smb.Name{Parent: rootInode, Base: "moved"}, SourceInode: dir.Object}); err != nil {
		t.Fatal(err)
	}
	if path, err := e.PathOf(t.Context(), file.Object); err != nil || path != "moved/file" {
		t.Fatalf("path after rename = %q, %v", path, err)
	}
	_, err := e.PathOf(t.Context(), 0)
	requireError(t, err, smb.ErrInvalidParameter)
	_, err = e.PathOf(t.Context(), file.Object+100)
	requireError(t, err, smb.ErrNameNotFound)
}

func TestStorageDirectoryCookieSurvivesRemoval(t *testing.T) {
	e := newFixture(t).open()
	dir := create(t, e, "dir", smb.KindDirectory)
	a := create(t, e, "dir/a", smb.KindFile)
	for _, name := range []string{"b", "c", "d"} {
		create(t, e, "dir/"+name, smb.KindFile)
	}
	page, err := e.ReadDir(t.Context(), dir.Object, 0, 2)
	if err != nil || len(page) != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	cookie := page[1].Next
	if err = e.Remove(t.Context(), a.Name, a.Object); err != nil {
		t.Fatal(err)
	}
	page, err = e.ReadDir(t.Context(), dir.Object, cookie, 2)
	if err != nil || len(page) != 2 || page[0].Name != "c" || page[1].Name != "d" {
		t.Fatalf("continuation skipped entries: %+v, %v", page, err)
	}
	page, err = e.ReadDir(t.Context(), dir.Object, page[1].Next, 2)
	if err != nil || len(page) != 0 {
		t.Fatalf("exhaustion = %+v, %v", page, err)
	}
}

func TestStorageReadDirWithConcurrentRemoval(t *testing.T) {
	e := newFixture(t).open()
	dir := create(t, e, "dir", smb.KindDirectory)
	var removed []smb.Resolved
	for i := range 80 {
		r := create(t, e, fmt.Sprintf("dir/band-%02d", i), smb.KindFile)
		if i%2 == 0 {
			removed = append(removed, r)
		}
	}
	done := make(chan error, 1)
	go func() {
		for _, r := range removed {
			if err := e.Remove(t.Context(), r.Name, r.Object); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	seen := make(map[string]bool)
	var cookie smb.Cookie
	for {
		page, err := e.ReadDir(t.Context(), dir.Object, cookie, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, entry := range page {
			seen[entry.Name] = true
			cookie = entry.Next
		}
	}
	if err := storageReceive(t, done); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 80; i += 2 {
		if name := fmt.Sprintf("band-%02d", i); !seen[name] {
			t.Errorf("skipped live entry %s", name)
		}
	}
}

// An append-only write checks EOF under the same inode lock as every length
// change. Each case holds a mutation inside the engine and starts an append
// at the old EOF; once the mutation completes the append must fail.
func TestStorageAppendWriteCoordinatesWithMutations(t *testing.T) {
	for _, mutation := range []string{"write", "truncate", "set size"} {
		t.Run(mutation, func(t *testing.T) { checkStorageAppendMutation(t, mutation) })
	}
}

func checkStorageAppendMutation(t *testing.T, mutation string) {
	t.Helper()
	f := newFixture(t)
	gate := newStorageGate(t)
	var armed atomic.Bool
	f.tune.hook = func(step string) error {
		if step == stepCommit && armed.CompareAndSwap(true, false) {
			gate.pause()
		}
		return nil
	}
	e := f.open()
	writeFile(t, e, "data", "seed")
	r := lookup(t, e, "data")
	seed := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	winner := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	appender := openFile(t, e, "data", smb.AccessRead|smb.AccessAppend)
	// A write into the stored chunk reads it first, under the inode lock.
	if mutation == "write" {
		holdChunks(f, "get", gate)
	} else {
		armed.Store(true)
	}
	first := make(chan error, 1)
	go func() {
		size := uint64(10)
		switch mutation {
		case "truncate":
			first <- e.Truncate(t.Context(), winner, size)
		case "set size":
			first <- e.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size})
		default:
			_, err := e.WriteAt(t.Context(), winner, []byte("winner"), 4)
			first <- err
		}
	}()
	storageReceive(t, gate.entered)
	second := make(chan error, 1)
	go func() {
		n, err := e.WriteAt(t.Context(), appender, []byte("stale"), 4)
		if n != 0 {
			err = errors.Join(err, fmt.Errorf("stale append wrote %d bytes", n))
		}
		second <- err
	}()
	gate.open()
	if err := storageReceive(t, first); err != nil {
		t.Fatal(err)
	}
	requireError(t, storageReceive(t, second), smb.ErrAccessDenied)
	want := "seedwinner"
	if mutation != "write" {
		want = "seed" + zeros(6)
	}
	requireContent(t, e, seed, want)
}

func TestStorageAppendAccessAndSelectedEOF(t *testing.T) {
	e := newFixture(t).open()
	writeFile(t, e, "data", "base payload")
	r := lookup(t, e, "data")
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	appender := openFile(t, e, "data", smb.AccessAppend)
	_, err := e.ReadAt(t.Context(), appender, make([]byte, 1), 0)
	requireError(t, err, smb.ErrAccessDenied)
	for _, data := range [][]byte{nil, []byte("overwrite")} {
		n, writeErr := e.WriteAt(t.Context(), appender, data, 0)
		if n != 0 {
			t.Fatalf("denied write = %d", n)
		}
		requireError(t, writeErr, smb.ErrAccessDenied)
	}
	requireError(t, e.Truncate(t.Context(), appender, 0), smb.ErrAccessDenied)
	a, err := e.GetAttr(t.Context(), r.Object)
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, e, appender, "!", a.Size)
	requireContent(t, e, h, "base payload!")

	// Combined flags let CREATE set the length without removing append checks.
	combined := openFile(t, e, "data", smb.AccessWrite|smb.AccessAppend)
	if err = e.Truncate(t.Context(), combined, 2); err != nil {
		t.Fatal(err)
	}
	_, err = e.WriteAt(t.Context(), combined, []byte("bad"), 0)
	requireError(t, err, smb.ErrAccessDenied)
	writeAt(t, e, combined, "", 2)
	writeAt(t, e, combined, "!", 20)
	writeAt(t, e, h, "", 1000)
	writeAt(t, e, h, "OK", 0)
	want := "OK" + zeros(18) + "!"
	requireContent(t, e, h, want)
	flush(t, e, h)
	requireContent(t, e, h, want)
}

func TestStorageExplicitTimestampSurvivesFlush(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	r := create(t, e, "data", smb.KindFile)
	writer := openFile(t, e, "data", smb.AccessWrite)
	other := openFile(t, e, "data", smb.AccessRead)
	writeAt(t, e, writer, strings.Repeat("dirty", 5), 0)
	stamp := time.Unix(1000000000, 123456700).UTC()
	if err := e.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp}); err != nil {
		t.Fatal(err)
	}
	requireModified := func(e *Engine) {
		t.Helper()
		a, err := e.GetAttr(t.Context(), r.Object)
		if err != nil || !a.Modified.Equal(stamp) {
			t.Fatalf("modified = %v, %v; want %v", a.Modified, err, stamp)
		}
	}
	requireModified(e)
	if err := e.Flush(t.Context(), other, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	requireModified(e)
	closeFile(t, e, writer)
	closeFile(t, e, other)
	shutdown(t, e)
	requireModified(f.open())
}

func TestStorageLaterWritesUpdateExplicitTimesButKeepCreationTime(t *testing.T) {
	e := newFixture(t).open()
	r := create(t, e, "data", smb.KindFile)
	initial := r.Attr.Created
	h := openFile(t, e, "data", smb.AccessWrite)
	stamp := time.Unix(1000000000, 123456700)
	if err := e.SetAttr(t.Context(), r.Object, smb.AttrChange{Modified: &stamp, Changed: &stamp}); err != nil {
		t.Fatal(err)
	}
	writeAt(t, e, h, "data", 0)
	if err := e.Flush(t.Context(), h, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	a, err := e.GetAttr(t.Context(), r.Object)
	if err != nil || !a.Modified.After(stamp) || !a.Changed.After(stamp) || !a.Created.Equal(initial) {
		t.Fatalf("later attrs = %+v, %v", a, err)
	}
}

func TestStorageFlushTimesStayUnchangedAfterLastClose(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	r := create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessWrite)
	writeAt(t, e, h, "abcdef", 0)
	flush(t, e, h)
	open, err := e.GetAttr(t.Context(), r.Object)
	if err != nil {
		t.Fatal(err)
	}
	closeFile(t, e, h)
	closed, err := e.GetAttr(t.Context(), r.Object)
	if err != nil || !open.Modified.Equal(closed.Modified) || !open.Changed.Equal(closed.Changed) {
		t.Fatalf("times changed on close: open=%+v, closed=%+v, %v", open, closed, err)
	}
	shutdown(t, e)
	restarted, err := f.open().GetAttr(t.Context(), r.Object)
	if err != nil || !open.Modified.Equal(restarted.Modified) || !open.Changed.Equal(restarted.Changed) {
		t.Fatalf("times changed on restart: open=%+v, restarted=%+v, %v", open, restarted, err)
	}
}

func TestStorageSizeCap(t *testing.T) {
	e := newFixture(t).open()
	r := create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	data := "retained across two chunks"
	writeAt(t, e, h, data, 0)
	before, err := e.GetAttr(t.Context(), r.Object)
	if err != nil {
		t.Fatal(err)
	}
	// A cap at or above EOF changes neither length nor times.
	for _, limit := range []uint64{uint64(len(data)), 8192} {
		if err = e.SetAttr(t.Context(), r.Object, smb.AttrChange{SizeCap: &limit}); err != nil {
			t.Fatal(err)
		}
		after, attrErr := e.GetAttr(t.Context(), r.Object)
		if attrErr != nil || after != before {
			t.Fatalf("cap %d changed attributes: %+v, want %+v; %v", limit, after, before, attrErr)
		}
	}
	limit, oversized := uint64(3), uint64(1<<63)
	requireError(t, e.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &limit, SizeCap: &limit}), smb.ErrInvalidParameter)
	requireError(t, e.SetAttr(t.Context(), r.Object, smb.AttrChange{SizeCap: &oversized}), smb.ErrFileTooLarge)
	if err = e.SetAttr(t.Context(), r.Object, smb.AttrChange{SizeCap: &limit}); err != nil {
		t.Fatal(err)
	}
	flush(t, e, h)
	requireContent(t, e, h, "ret")
	directory := create(t, e, "dir", smb.KindDirectory)
	limit = 0
	requireError(t, e.SetAttr(t.Context(), directory.Object, smb.AttrChange{SizeCap: &limit}), smb.ErrIsDirectory)
}

func TestStorageStatFSConfiguredCapacityAndDefaultCap(t *testing.T) {
	const used = 40
	for _, capacity := range []uint64{0, 10, 1000, 2000000000000} {
		f := newFixture(t)
		e := storageOpen(t, f, Options{Capacity: capacity})
		writeFile(t, e, "data", strings.Repeat("x", used))
		create(t, e, "dir", smb.KindDirectory)
		space, err := e.StatFS(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		wantCapacity, wantFree := capacity, capacity-min(capacity, used)
		if capacity == 0 {
			wantCapacity, wantFree = used+1<<40, 1<<40
		}
		if space.Capacity != wantCapacity || space.Free != wantFree || space.Available != space.Free || space.VolumeID == 0 {
			t.Fatalf("capacity %d: space = %+v", capacity, space)
		}
		shutdown(t, e)
		again, err := storageOpen(t, f, Options{Capacity: capacity}).StatFS(t.Context())
		if err != nil || again != space {
			t.Fatalf("after restart space = %+v, %v; want %+v", again, err, space)
		}
	}
}

func TestStorageFlushThroughNonWriterCommitsData(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	r := create(t, e, "data", smb.KindFile)
	writer := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	other := openFile(t, e, "data", smb.AccessRead)
	payload := "durable payload across chunks"
	writeAt(t, e, writer, payload, 0)
	flush(t, e, other)
	if size := storageCount(t, e, `SELECT size FROM files WHERE id = ?`, r.Object); size != len(payload) {
		t.Fatalf("committed size = %d", size)
	}
	if n := len(f.bucket.keys(chunkPrefix)); n != 2 {
		t.Fatalf("%d chunks in the bucket, want 2", n)
	}
	f.dir = snapshot(t, e)
	kill(t, e)
	if got := readFile(t, f.open(), "data"); got != payload {
		t.Fatalf("after a crash = %q", got)
	}
}

func TestStorageFlushUploadErrorReachesNonWriter(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "data", smb.KindFile)
	writer := openFile(t, e, "data", smb.AccessWrite)
	other := openFile(t, e, "data", smb.AccessRead)
	attempts := failChunkPuts(f)
	writeAt(t, e, writer, "uncommitted", 0)
	requireError(t, e.Flush(t.Context(), other, smb.SyncData), smb.ErrIO)
	if attempts.Load() == 0 {
		t.Fatal("flush did not attempt an upload")
	}
	requireContent(t, e, other, "uncommitted")
	f.bucket.setFault(nil)
	flush(t, e, other)
	closeFile(t, e, writer)
	closeFile(t, e, other)
	if got := readFile(t, e, "data"); got != "uncommitted" {
		t.Fatalf("after retry = %q", got)
	}
}

// Close never waits on S3: dirty data stays for the next FLUSH, which
// commits it once S3 works again.
func TestStorageCloseAlwaysReleasesReference(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "failing", smb.KindFile)
	h := openFile(t, e, "failing", smb.AccessWrite)
	failChunkPuts(f)
	writeAt(t, e, h, "data", 0)
	before := chunkRequests(f.bucket)
	closeFile(t, e, h)
	requireError(t, e.Close(t.Context(), h), smb.ErrInvalidHandle)
	if n := chunkRequests(f.bucket); n != before {
		t.Fatalf("close made %d chunk requests", n-before)
	}
	f.bucket.setFault(nil)

	create(t, e, "canceled", smb.KindFile)
	h = openFile(t, e, "canceled", smb.AccessWrite)
	writeAt(t, e, h, "data", 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requireError(t, e.Close(ctx, h), context.Canceled)
	requireError(t, e.Close(t.Context(), h), smb.ErrInvalidHandle)
	if got := readFile(t, e, "canceled"); got != "data" {
		t.Fatalf("canceled close lost data: %q", got)
	}

	// The closes kept the bytes, so a later flush commits them.
	for _, name := range []string{"failing", "canceled"} {
		h = openFile(t, e, name, smb.AccessRead)
		flush(t, e, h)
		closeFile(t, e, h)
	}
	if got := readFile(t, e, "failing"); got != "data" {
		t.Fatalf("close lost data: %q", got)
	}
	if n := storageInodes(e); n != 0 {
		t.Fatalf("%d closed references retained", n)
	}
}

func TestStorageTruncateCannotResurrectBufferedBytes(t *testing.T) {
	data := "abcdefghijklmnopqrstuvwxyz"
	for _, size := range []uint64{0, 2, 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newFixture(t)
			e := f.open()
			r := create(t, e, "data", smb.KindFile)
			a := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
			b := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
			writeAt(t, e, a, data, 0)
			if err := e.Truncate(t.Context(), b, size); err != nil {
				t.Fatal(err)
			}
			flush(t, e, a)
			requireContent(t, e, b, data[:size])
			writeAt(t, e, a, "new", size)
			if err := e.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}); err != nil {
				t.Fatal(err)
			}
			if err := e.Flush(t.Context(), a, smb.SyncFull); err != nil {
				t.Fatal(err)
			}
			closeFile(t, e, a)
			closeFile(t, e, b)
			if got := readFile(t, e, "data"); got != data[:size] {
				t.Fatalf("reopened = %q", got)
			}
			shutdown(t, e)
			if got := readFile(t, f.open(), "data"); got != data[:size] {
				t.Fatalf("after restart = %q", got)
			}
		})
	}
}

func TestStorageLookupAndDirectoryReportBufferedLength(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	r := create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessWrite)
	writeAt(t, e, h, "buffered across chunks", 0)
	const size = 22
	resolved, err := e.Lookup(t.Context(), "data")
	if err != nil || resolved.Attr.Size != size {
		t.Fatalf("lookup = %+v, %v", resolved, err)
	}
	entries, err := e.ReadDir(t.Context(), rootInode, 0, 10)
	if err != nil || len(entries) != 1 || entries[0].Attr.Size != size {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	a, err := e.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != size || a.AllocationSize < size {
		t.Fatalf("attr = %+v, %v", a, err)
	}
	if n := chunkRequests(f.bucket); n != 0 {
		t.Fatalf("attribute queries made %d chunk requests", n)
	}
}

// Attribute queries do not take the inode's I/O lock, so a slow upload or a
// cold read must not block them, on the same file or on others.
func TestStorageMetadataQueriesDoNotWaitForInodeIO(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(map[bool]string{true: "upload", false: "cold_read"}[upload], func(t *testing.T) {
			f := newFixture(t)
			e := f.open()
			r := create(t, e, "data", smb.KindFile)
			create(t, e, "other", smb.KindFile)
			h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
			otherHandle := openFile(t, e, "other", smb.AccessWrite)
			writeAt(t, e, h, "data", 0)
			gate := newStorageGate(t)
			slow := make(chan error, 1)
			if upload {
				holdChunks(f, "put", gate)
				go func() { slow <- e.Flush(t.Context(), h, smb.SyncData) }()
			} else {
				flush(t, e, h)
				holdChunks(f, "get", gate)
				go func() {
					_, err := e.ReadAt(t.Context(), h, make([]byte, 4), 0)
					slow <- err
				}()
			}
			storageReceive(t, gate.entered)
			queries := make(chan error, 1)
			go func() {
				_, err := e.GetAttr(t.Context(), r.Object)
				if err == nil {
					_, err = e.Lookup(t.Context(), "data")
				}
				if err == nil {
					_, err = e.ReadDir(t.Context(), rootInode, 0, 10)
				}
				if err == nil {
					_, err = e.WriteAt(t.Context(), otherHandle, []byte("other"), 0)
				}
				queries <- err
			}()
			if err := storageReceive(t, queries); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-slow:
				t.Fatalf("I/O finished while S3 was held: %v", err)
			default:
			}
			gate.open()
			if err := storageReceive(t, slow); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// GetAttr, Lookup and ReadDir report the written length while a flush
// commits it.
func TestStorageAttributeLengthAcrossFlushCompletion(t *testing.T) {
	e := newFixture(t).open()
	r := create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessWrite)
	for size := range uint64(40) {
		writeAt(t, e, h, "x", size)
		done := make(chan error, 1)
		go func() { done <- e.Flush(t.Context(), h, smb.SyncData) }()
		a, err := e.GetAttr(t.Context(), r.Object)
		if err != nil || a.Size != size+1 {
			t.Fatalf("attr = %+v, %v; want size %d", a, err, size+1)
		}
		resolved, err := e.Lookup(t.Context(), "data")
		if err != nil || resolved.Attr.Size != size+1 {
			t.Fatalf("lookup = %+v, %v; want size %d", resolved, err, size+1)
		}
		entries, err := e.ReadDir(t.Context(), rootInode, 0, 10)
		if err != nil || len(entries) != 1 || entries[0].Attr.Size != size+1 {
			t.Fatalf("entries = %+v, %v; want size %d", entries, err, size+1)
		}
		if err = storageReceive(t, done); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStorageIOUsesRetainedKindAndLength(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "dir", smb.KindDirectory)
	dh := openFile(t, e, "dir", smb.AccessRead|smb.AccessWrite)
	_, err := e.ReadAt(t.Context(), dh, make([]byte, 1), 0)
	requireError(t, err, smb.ErrIsDirectory)
	_, err = e.WriteAt(t.Context(), dh, []byte("x"), 0)
	requireError(t, err, smb.ErrIsDirectory)
	requireError(t, e.Truncate(t.Context(), dh, 0), smb.ErrIsDirectory)
	closeFile(t, e, dh)

	create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	for range 5 {
		writeAt(t, e, h, "buffered data", 0)
		requireContent(t, e, h, "buffered data")
	}
	if n := chunkRequests(f.bucket); n != 0 {
		t.Fatalf("buffered I/O made %d chunk requests", n)
	}
}

func TestStorageBaseOffsetsAndZeroFilledExtensions(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	other := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	writeAt(t, e, h, "abc", 20)
	requireContent(t, e, other, zeros(20)+"abc")
	if err := e.Truncate(t.Context(), other, 40); err != nil {
		t.Fatal(err)
	}
	want := zeros(20) + "abc" + zeros(17)
	requireContent(t, e, h, want)
	flush(t, e, h)
	requireContent(t, e, other, want)
	closeFile(t, e, h)
	closeFile(t, e, other)
	shutdown(t, e)
	if got := readFile(t, f.open(), "data"); got != want {
		t.Fatalf("after restart = %q", got)
	}
}

// Handles carry no SMB share or lock policy: any number may write, and
// closing one leaves the others working.
func TestStorageReferencesHaveNoOpenOrLockPolicy(t *testing.T) {
	e := newFixture(t).open()
	create(t, e, "data", smb.KindFile)
	a := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	b := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	writeAt(t, e, a, "first", 0)
	writeAt(t, e, b, "other", 0)
	if err := e.Flush(t.Context(), b, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	if err := e.Truncate(t.Context(), a, 3); err != nil {
		t.Fatal(err)
	}
	closeFile(t, e, a)
	requireError(t, e.Close(t.Context(), a), smb.ErrInvalidHandle)
	_, err := e.ReadAt(t.Context(), a, make([]byte, 1), 0)
	requireError(t, err, smb.ErrInvalidHandle)
	requireContent(t, e, b, "oth")
	closeFile(t, e, b)
	if n := storageInodes(e); n != 0 {
		t.Fatalf("%d unused inode states retained", n)
	}
}

func TestStorageConcurrentInodeWritesAndQueries(t *testing.T) {
	e := newFixture(t).open()
	r := create(t, e, "data", smb.KindFile)
	a := openFile(t, e, "data", smb.AccessWrite)
	b := openFile(t, e, "data", smb.AccessWrite)
	var group sync.WaitGroup
	errs := make(chan error, 3)
	for _, w := range []struct {
		h      smb.Handle
		fill   string
		offset uint64
	}{{a, "a", 0}, {b, "b", 100}} {
		group.Go(func() {
			for i := range uint64(10) {
				if _, err := e.WriteAt(t.Context(), w.h, []byte(strings.Repeat(w.fill, 10)), w.offset+i*10); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	group.Go(func() {
		for range 20 {
			if _, err := e.GetAttr(t.Context(), r.Object); err != nil {
				errs <- err
				return
			}
		}
	})
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	attr, err := e.GetAttr(t.Context(), r.Object)
	if err != nil || attr.Size != 200 {
		t.Fatalf("size = %d, %v", attr.Size, err)
	}
	closeFile(t, e, a)
	closeFile(t, e, b)
	if got, want := readFile(t, e, "data"), strings.Repeat("a", 100)+strings.Repeat("b", 100); got != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

func TestStorageAccessAndCancellation(t *testing.T) {
	e := newFixture(t).open()
	r := create(t, e, "data", smb.KindFile)
	reader := openFile(t, e, "data", smb.AccessRead)
	_, err := e.WriteAt(t.Context(), reader, []byte("x"), 0)
	requireError(t, err, smb.ErrAccessDenied)
	requireError(t, e.Truncate(t.Context(), reader, 0), smb.ErrAccessDenied)
	writer := openFile(t, e, "data", smb.AccessWrite)
	_, err = e.ReadAt(t.Context(), writer, make([]byte, 1), 0)
	requireError(t, err, smb.ErrAccessDenied)
	requireError(t, e.Flush(t.Context(), writer, smb.SyncMode(9)), smb.ErrInvalidParameter)
	_, err = e.Open(t.Context(), r.Object, smb.Access(0x80))
	requireError(t, err, smb.ErrInvalidParameter)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = e.Lookup(ctx, "data")
	requireError(t, err, context.Canceled)
	_, err = e.Open(ctx, r.Object, smb.AccessRead)
	requireError(t, err, context.Canceled)
	_, err = e.WriteAt(ctx, writer, []byte("x"), 0)
	requireError(t, err, context.Canceled)
	_, err = e.ReadAt(ctx, reader, make([]byte, 1), 0)
	requireError(t, err, context.Canceled)
}

func TestStorageFileSizeBoundary(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	r := create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	writeAt(t, e, h, "abc", 0)
	for _, size := range []uint64{maxFileSize, maxFileSize + 1, math.MaxUint64} {
		requireError(t, e.Truncate(t.Context(), h, size), smb.ErrFileTooLarge)
		requireError(t, e.SetAttr(t.Context(), r.Object, smb.AttrChange{Size: &size}), smb.ErrFileTooLarge)
		requireError(t, e.SetAttr(t.Context(), r.Object, smb.AttrChange{SizeCap: &size}), smb.ErrFileTooLarge)
		_, err := e.WriteAt(t.Context(), h, []byte("x"), size-1)
		requireError(t, err, smb.ErrFileTooLarge)
		n, err := e.ReadAt(t.Context(), h, make([]byte, 1), size-1)
		if n != 0 || smb.StatusFromError(err) != smb.StatusEndOfFile {
			t.Fatalf("read at %d = %d, %v", size-1, n, err)
		}
	}
	a, err := e.GetAttr(t.Context(), r.Object)
	if err != nil || a.Size != 3 || chunkRequests(f.bucket) != 0 {
		t.Fatalf("invalid range changed data: %+v, %v, chunk requests %d", a, err, chunkRequests(f.bucket))
	}
	if err = e.Truncate(t.Context(), h, maxFileSize-1); err != nil {
		t.Fatal(err)
	}
	writeAt(t, e, h, "z", maxFileSize-2)
	data := make([]byte, 2)
	n, err := e.ReadAt(t.Context(), h, data, maxFileSize-2)
	if n != 1 || data[0] != 'z' || !errors.Is(err, io.EOF) {
		t.Fatalf("read across the last byte = %q, %d, %v", data, n, err)
	}
	if err = e.Truncate(t.Context(), h, 3); err != nil {
		t.Fatal(err)
	}
	flush(t, e, h)
	requireContent(t, e, h, "abc")
	if n := len(f.bucket.keys(chunkPrefix)); n != 1 {
		t.Fatalf("%d chunks in the bucket, want 1", n)
	}
}

func TestStorageAttributesPersistAcrossRestart(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	r := create(t, e, "data", smb.KindFile)
	dir := create(t, e, "dir", smb.KindDirectory)
	stamp := time.Unix(-123456, 123456789).UTC()
	// A file never gets the directory bit, and a directory keeps it.
	fileBits, dirBits := uint32(0x32), uint32(0x02)
	if err := e.SetAttr(t.Context(), r.Object, smb.AttrChange{Created: &stamp, Accessed: &stamp, Modified: &stamp, Changed: &stamp, Attributes: &fileBits}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetAttr(t.Context(), dir.Object, smb.AttrChange{Attributes: &dirBits}); err != nil {
		t.Fatal(err)
	}
	check := func(e *Engine) {
		t.Helper()
		a, err := e.GetAttr(t.Context(), r.Object)
		if err != nil || !a.Created.Equal(stamp) || !a.Accessed.Equal(stamp) || !a.Modified.Equal(stamp) || !a.Changed.Equal(stamp) || a.Attributes != 0x22 {
			t.Fatalf("attr = %+v, %v", a, err)
		}
		d, err := e.GetAttr(t.Context(), dir.Object)
		if err != nil || d.Attributes != dirBits|attributeDirectory {
			t.Fatalf("directory attr = %+v, %v", d, err)
		}
	}
	check(e)
	shutdown(t, e)
	check(f.open())
}

// A SyncFull flush commits every file's dirty data, not only its own.
func TestStorageSyncFullCommitsEveryFile(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	want := map[string]string{"a": strings.Repeat("a", 20), "b": strings.Repeat("b", 20)}
	handles := make(map[string]smb.Handle)
	for name, data := range want {
		create(t, e, name, smb.KindFile)
		handles[name] = openFile(t, e, name, smb.AccessWrite)
		writeAt(t, e, handles[name], data, 0)
	}
	if err := e.Flush(t.Context(), handles["a"], smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	f.dir = snapshot(t, e)
	kill(t, e)
	if got := tree(t, f.open()); !maps.Equal(got, want) {
		t.Fatalf("after a crash = %v", got)
	}
}

// Writes beyond the RAM budget go up early. Reads see them before and after
// FLUSH, and after a restart.
func TestStorageManySmallWritesBeyondTheRAMBudget(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "big", smb.KindFile)
	create(t, e, "other", smb.KindFile)
	h := openFile(t, e, "big", smb.AccessRead|smb.AccessWrite)
	o := openFile(t, e, "other", smb.AccessRead|smb.AccessWrite)
	var big, other strings.Builder
	for i := range uint64(67) {
		piece := fmt.Sprintf("%03d", i)
		writeAt(t, e, h, piece, 3*i)
		writeAt(t, e, o, piece[2:], i)
		big.WriteString(piece)
		other.WriteString(piece[2:])
	}
	if n := len(f.bucket.keys(chunkPrefix)); n == 0 {
		t.Fatal("no chunk went up early")
	}
	writeAt(t, e, h, "XY", 5)
	want := big.String()[:5] + "XY" + big.String()[7:]
	requireContent(t, e, h, want)
	requireContent(t, e, o, other.String())
	flush(t, e, h)
	requireContent(t, e, h, want)
	closeFile(t, e, h)
	closeFile(t, e, o)
	shutdown(t, e)
	if got := tree(t, f.open()); !maps.Equal(got, map[string]string{"big": want, "other": other.String()}) {
		t.Fatalf("after restart = %q", got)
	}
}

// Small reads of a stored chunk cost one GET while the chunk stays in the
// read cache. A write into a cached chunk must not change the cached bytes.
func TestStorageReadCacheFetchesEachChunkOnce(t *testing.T) {
	f := newFixture(t)
	f.tune.readChunks = 2
	e := f.open()
	const data = "0123456789abcdefghijklmnopqrstuvABCDEFGHIJKLMNOP"
	writeFile(t, e, "data", data)
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	readBytes := func(from, to uint64) {
		t.Helper()
		b := make([]byte, 1)
		for i := from; i < to; i++ {
			if _, err := e.ReadAt(t.Context(), h, b, i); err != nil || b[0] != data[i] {
				t.Fatalf("byte %d = %q, %v", i, b, err)
			}
		}
	}
	gets := func(want int) {
		t.Helper()
		if n := chunkRequests(f.bucket); n != want {
			t.Fatalf("%d chunk requests, want %d", n, want)
		}
	}
	base := chunkRequests(f.bucket)
	readBytes(0, 16)
	gets(base + 1)
	readBytes(0, uint64(len(data)))
	gets(base + 3)
	// Two chunks fit, so the first one was dropped.
	readBytes(0, 1)
	gets(base + 4)
	var second string
	if err := e.db.QueryRowContext(t.Context(), `SELECT name FROM chunks WHERE idx = 1`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	readBytes(16, 17)
	writeAt(t, e, h, "XY", 16)
	if cached := string(e.cache.get(second)); cached != data[16:32] {
		t.Fatalf("cached chunk changed to %q", cached)
	}
	requireContent(t, e, h, data[:16]+"XY"+data[18:])
	closeFile(t, e, h)
	if got := readFile(t, e, "data"); got != data[:16]+"XY"+data[18:] {
		t.Fatalf("read %q", got)
	}
}

// Open, Close and Remove must not wait for a file's I/O lock, which an
// upload holds for the whole of an S3 outage: CREATE opens a file while it
// guards the file's folder, and CLOSE closes and removes it. The file is
// dropped once the upload ends.
func TestStorageOpenCloseAndRemoveDoNotWaitForUploads(t *testing.T) {
	f := newFixture(t)
	bucket := &pausable{objects: f.bucket}
	e, err := open(t.Context(), Options{Dir: f.dir}, bucket, f.tune)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kill(t, e) })
	r := create(t, e, "data", smb.KindFile)
	h := openFile(t, e, "data", smb.AccessRead|smb.AccessWrite)
	other := openFile(t, e, "data", smb.AccessRead)
	writeAt(t, e, h, "uploaded during an outage", 0)
	bucket.gate.Lock()
	reopen := sync.OnceFunc(bucket.gate.Unlock)
	defer reopen()
	flushed := make(chan error, 1)
	go func() { flushed <- e.Flush(t.Context(), h, smb.SyncData) }()
	// Wait until the FLUSH holds the I/O lock and waits on S3.
	st, unpin := e.pin(r.Object)
	for st.mu.TryLock() {
		st.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	unpin()
	done := make(chan error, 1)
	go func() {
		third, err := e.Open(t.Context(), r.Object, smb.AccessRead)
		if err != nil {
			done <- err
			return
		}
		done <- errors.Join(e.Close(t.Context(), third), e.Close(t.Context(), other), e.Remove(t.Context(), r.Name, r.Object), e.Close(t.Context(), h))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open, close and remove waited for the upload")
	}
	reopen()
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if got, err := e.Lookup(t.Context(), "data"); err != nil || got.Exists {
		t.Fatal("removed file still in the namespace", err)
	}
	// The drop runs once the FLUSH has let go of the I/O lock.
	for deadline := time.Now().Add(5 * time.Second); countRows(t, e, "files") != 1 || countRows(t, e, "chunks") != 0; {
		if time.Now().After(deadline) {
			t.Fatal("the removed file was not dropped after the upload")
		}
		time.Sleep(time.Millisecond)
	}
}
