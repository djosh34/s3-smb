// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// waitFor polls cond until it holds or ten seconds pass.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A replaced chunk is deleted once the oldest of the 4 kept copies was
// captured after it went to the trash, and not one copy earlier.
func TestTrashWaitsForTheKeptCopies(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", strings.Repeat("a", 16))
	old := f.bucket.keys(chunkPrefix)
	writeFile(t, e, "f", strings.Repeat("b", 16))
	for range 3 {
		copyNow(t, e)
		if _, ok := f.bucket.data(old[0]); !ok {
			t.Fatal("deleted while a kept copy may still use it")
		}
	}
	copyNow(t, e)
	if _, ok := f.bucket.data(old[0]); ok {
		t.Fatal("not deleted after four newer copies")
	}
	if n := countRows(t, e, "trash"); n != 0 {
		t.Fatalf("%d trash rows left", n)
	}
	if n := len(f.bucket.keys(copyPrefix)); n != 4 {
		t.Fatalf("%d copies kept", n)
	}
	if got := readFile(t, e, "f"); got != strings.Repeat("b", 16) {
		t.Fatalf("read %q", got)
	}
	checkCopies(t, f.bucket)
}

func TestCleanupNeverDeletesLiveOrUnknownChunks(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "live")
	live := f.bucket.keys(chunkPrefix)
	if err := f.bucket.put(t.Context(), chunkPrefix+"unknown", []byte("leaked")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(t.Context(), `INSERT INTO trash (name, seq) VALUES (?, 0)`, strings.TrimPrefix(live[0], chunkPrefix)); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		copyNow(t, e)
	}
	for _, key := range append(live, chunkPrefix+"unknown") {
		if _, ok := f.bucket.data(key); !ok {
			t.Fatalf("%s was deleted", key)
		}
	}
	if got := readFile(t, e, "f"); got != "live" {
		t.Fatalf("read %q", got)
	}
}

// With a small copy count and interval, the copy loop alone runs the delete
// path. Copies run even when nothing changed.
func TestCopyLoopDeletesTrash(t *testing.T) {
	f := newFixture(t)
	f.tune.copiesKept = 2
	f.tune.copyInterval = 10 * time.Millisecond
	e := f.open()
	writeFile(t, e, "f", "old")
	old := f.bucket.keys(chunkPrefix)
	writeFile(t, e, "f", "new")
	waitFor(t, "the trashed chunk to go", func() bool {
		_, ok := f.bucket.data(old[0])
		return !ok
	})
	e.commitMu.Lock()
	seq := e.captureSeq
	e.commitMu.Unlock()
	waitFor(t, "copies with nothing changed", func() bool {
		e.commitMu.Lock()
		defer e.commitMu.Unlock()
		return e.captureSeq >= seq+3
	})
	shutdown(t, e)
	checkCopies(t, f.bucket)
}

func TestCopyRetriesTheSameName(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	var keys []string
	f.bucket.setFault(func(op, key string) error {
		if op == "put" && strings.HasPrefix(key, copyPrefix) {
			keys = append(keys, key)
			if len(keys) < 3 {
				return errors.New("injected copy failure")
			}
		}
		return nil
	})
	copyNow(t, e)
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("attempts %v", keys)
	}
	if !slices.Contains(f.bucket.keys(copyPrefix), keys[0]) {
		t.Fatal("the copy did not land")
	}
	var landed bool
	c, _ := parseCopy(keys[0])
	if err := e.db.QueryRowContext(t.Context(), `SELECT landed FROM copies WHERE seq = ?`, c.seq).Scan(&landed); err != nil || !landed {
		t.Fatalf("attempt row landed=%v, %v", landed, err)
	}
}

func TestStopWhenCopiesStopLanding(t *testing.T) {
	for _, hang := range []bool{false, true} {
		f := newFixture(t)
		f.tune.copyInterval = 10 * time.Millisecond
		f.tune.stopAge = 100 * time.Millisecond
		e := f.open()
		release := make(chan struct{})
		f.bucket.setFault(func(op, key string) error {
			if op != "put" || !strings.HasPrefix(key, copyPrefix) {
				return nil
			}
			if hang {
				<-release
			}
			return errors.New("injected copy failure")
		})
		select {
		case <-e.Dead():
		case <-time.After(10 * time.Second):
			t.Fatal("the engine did not stop")
		}
		close(release)
		if err := e.Err(); err == nil || !strings.Contains(err.Error(), "newest database copy") {
			t.Fatalf("hang=%v: error %v", hang, err)
		}
	}
}

// A long trash backlog stops at the next copy's deadline instead of holding
// copies up, and later cleanups finish it.
func TestCleanupStopsWhenTheNextCopyIsDue(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	for range 20 {
		writeFile(t, e, "f", "replaced again")
	}
	for range 3 {
		copyNow(t, e)
	}
	// The copy loop sleeps for a day, so only this cleanup reads the interval.
	e.tune.copyInterval = 100 * time.Millisecond
	f.bucket.setFault(func(op, key string) error {
		if op == "delete" && strings.HasPrefix(key, chunkPrefix) {
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	})
	start := time.Now()
	copyNow(t, e)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("the copy took %s", elapsed)
	}
	if n := countRows(t, e, "trash"); n == 0 {
		t.Fatal("the whole backlog went in one cleanup")
	}
	waitFor(t, "the backlog to clear", func() bool {
		copyNow(t, e)
		return countRows(t, e, "trash") == 0
	})
}
