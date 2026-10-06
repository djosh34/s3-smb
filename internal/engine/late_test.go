// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func holdPrefix(op, prefix string) func(string, string) bool {
	return func(o, key string) bool { return o == op && strings.HasPrefix(key, prefix) }
}

// A chunk PUT whose FLUSH failed lands after the retry committed another
// name. It is an unknown object, never used and never deleted.
func TestLateChunkPut(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "f", smb.KindFile)
	h := openFile(t, e, "f", smb.AccessRead|smb.AccessWrite)
	writeAt(t, e, h, "data", 0)
	f.bucket.hold(holdPrefix("put", chunkPrefix))
	requireError(t, e.Flush(t.Context(), h, smb.SyncData), errHeld)
	f.bucket.hold(nil)
	flush(t, e, h)
	closeFile(t, e, h)
	before := f.bucket.keys(chunkPrefix)
	f.bucket.release()
	after := f.bucket.keys(chunkPrefix)
	if len(after) != len(before)+1 {
		t.Fatalf("chunks %v, then %v", before, after)
	}
	for range 6 {
		copyNow(t, e)
	}
	if got := f.bucket.keys(chunkPrefix); !slices.Equal(got, after) {
		t.Fatalf("chunks %v, want %v", got, after)
	}
	shutdown(t, e)
	requireTree(t, f.open(), map[string]string{"f": "data"})
}

// A trash DELETE that times out is retried by a later cleanup, here the one
// after the next start copy. When the first one lands late, it can only hit
// that dead name.
func TestLateDelete(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "old")
	old := f.bucket.keys(chunkPrefix)
	writeFile(t, e, "f", "new")
	for range 3 {
		copyNow(t, e)
	}
	f.bucket.hold(holdPrefix("delete", chunkPrefix))
	if err := e.makeCopy(t.Context(), 0); err == nil {
		t.Fatal("the cleanup did not see its DELETE fail")
	}
	f.bucket.hold(nil)
	if n := countRows(t, e, "trash"); n != 1 {
		t.Fatalf("%d trash rows", n)
	}
	shutdown(t, e)
	// The start copy's cleanup retries the DELETE.
	e = f.open()
	if _, ok := f.bucket.data(old[0]); ok || countRows(t, e, "trash") != 0 {
		t.Fatal("the retry did not delete the trashed chunk")
	}
	writeFile(t, e, "g", "written before the late DELETE lands")
	f.bucket.release()
	requireTree(t, e, map[string]string{"f": "new", "g": "written before the late DELETE lands"})
	checkCopies(t, f.bucket)
}

// A copy upload that seemed to fail is retried under the same name with the
// same bytes, and the first attempt landing late changes nothing. A copy
// still in flight when the run died lands below the next start copy.
func TestLateCopies(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "v1")
	// The first attempt is held; the retry lands.
	f.bucket.hold(holdPrefix("put", copyPrefix))
	attempts := 0
	f.bucket.setFault(func(op, key string) error {
		if op == "put" && strings.HasPrefix(key, copyPrefix) {
			if attempts++; attempts == 2 {
				f.bucket.hold(nil)
			}
		}
		return nil
	})
	copyNow(t, e)
	landed := newestCopy(t, f.bucket)
	data, _ := f.bucket.data(landed.key)
	f.bucket.mu.Lock()
	held := len(f.bucket.held)
	f.bucket.mu.Unlock()
	if attempts != 2 || held != 1 {
		t.Fatalf("%d attempts, %d held", attempts, held)
	}
	f.bucket.release()
	if late, _ := f.bucket.data(landed.key); string(late) != string(data) {
		t.Fatal("the late attempt changed the copy")
	}

	// The next copy is still in flight when the run dies.
	f.bucket.setFault(nil)
	writeFile(t, e, "f", "v2")
	f.bucket.hold(holdPrefix("put", copyPrefix))
	stopped := make(chan error, 1)
	go func() { stopped <- e.makeCopy(t.Context(), 0) }()
	waitFor(t, "the copy to be held", func() bool {
		f.bucket.mu.Lock()
		defer f.bucket.mu.Unlock()
		return len(f.bucket.held) > 0
	})
	inFlight := snapshot(t, e)
	kill(t, e)
	<-stopped
	f.bucket.hold(nil)
	f.dir = inFlight
	e = f.open()
	start := newestCopy(t, f.bucket)
	f.bucket.release()
	if late := newestCopy(t, f.bucket); late.key != start.key {
		t.Fatalf("the late copy %s outranks the start copy %s", late.key, start.key)
	}
	kill(t, e)
	lost := &fixture{t: t, bucket: f.bucket, dir: t.TempDir(), tune: testTuning()}
	requireTree(t, lost.open(), map[string]string{"f": "v2"})
}
