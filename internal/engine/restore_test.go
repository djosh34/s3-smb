// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"bytes"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func copyDir(t testing.TB, from string) string {
	t.Helper()
	to := t.TempDir()
	if err := os.CopyFS(to, os.DirFS(from)); err != nil {
		t.Fatal(err)
	}
	return to
}

func newestCopy(t testing.TB, b *memBucket) copyName {
	t.Helper()
	var newest copyName
	for _, key := range b.keys(copyPrefix) {
		c, ok := parseCopy(key)
		if !ok {
			t.Fatal(key)
		}
		if c.seq > newest.seq {
			newest = c
		}
	}
	return newest
}

func requireTree(t testing.TB, e *Engine, want map[string]string) {
	t.Helper()
	if got := tree(t, e); !maps.Equal(got, want) {
		t.Fatalf("tree = %q, want %q", got, want)
	}
}

// A local database behind the newest copy, with commits left in its WAL by a
// crash, is restored. A crash anywhere in that start leaves a state from
// which the next start still ends at the copy.
func TestCrashDuringRestoreWithLeftoverWAL(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "old")
	behind := snapshot(t, e)
	if info, err := os.Stat(filepath.Join(behind, databaseName+"-wal")); err != nil || info.Size() == 0 {
		t.Fatal("the snapshot has no WAL", err)
	}
	writeFile(t, e, "f", "new")
	writeFile(t, e, "g", "only in the copy")
	copyNow(t, e)
	shutdown(t, e)
	want := map[string]string{"f": "new", "g": "only in the copy"}
	fired := map[string]bool{}
	for at := 0; ; at++ {
		run := &fixture{t: t, bucket: f.bucket.clone(), dir: copyDir(t, behind), tune: testTuning()}
		c := &crasher{at: at}
		c.install(run)
		e, err := run.tryOpen()
		name := c.result()
		if name == "" {
			if err != nil {
				t.Fatal(err)
			}
			c.disarm()
			requireTree(t, e, want)
			break
		}
		fired[name] = true
		if e != nil {
			run.dir = snapshot(t, e)
			kill(t, e)
		}
		recoverAndCheck(t, run, name, []map[string]string{want}, []map[string]string{want})
	}
	for _, step := range []string{stepRestoreRemoved, stepRestoreRenamed, stepStartedTimeline} {
		if !fired[step] {
			t.Errorf("never crashed at %q", step)
		}
	}
}

func TestRestoreRemovesLeftoverWAL(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "in the copy")
	copyNow(t, e)
	writeFile(t, e, "f", "only in the WAL")
	dir := snapshot(t, e)
	kill(t, e)
	newest := newestCopy(t, f.bucket)
	r := &Engine{objs: f.bucket, dir: dir, log: slog.New(slog.DiscardHandler)}
	if err := r.restore(t.Context(), newest); err != nil {
		t.Fatal(err)
	}
	installed, err := openRoot(t, dir).ReadFile(databaseName)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := f.bucket.data(newest.key); !bytes.Equal(installed, want) {
		t.Fatal("the installed database is not the copy")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err = os.Stat(filepath.Join(dir, databaseName+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still there: %v", suffix, err)
		}
	}
}

// On a tie at the highest sequence, the local history wins. Without a local
// database, the copy with more commits wins.
func TestTieAtTheHighestSequence(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "local")
	copyNow(t, e)
	local := newestCopy(t, f.bucket)
	shutdown(t, e)

	other := newFixture(t)
	e = other.open()
	for range 5 {
		writeFile(t, e, "f", "foreign")
	}
	copyNow(t, e)
	foreign := newestCopy(t, other.bucket)
	shutdown(t, e)
	if foreign.counter <= local.counter {
		t.Fatalf("foreign counter %d, local %d", foreign.counter, local.counter)
	}
	for _, key := range other.bucket.keys(chunkPrefix) {
		data, _ := other.bucket.data(key)
		if err := f.bucket.put(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := other.bucket.data(foreign.key)
	if err := f.bucket.put(t.Context(), formatCopy(local.seq, foreign.counter, foreign.history), data); err != nil {
		t.Fatal(err)
	}

	lost := &fixture{t: t, bucket: f.bucket.clone(), dir: t.TempDir(), tune: testTuning()}
	requireTree(t, f.open(), map[string]string{"f": "local"})
	requireTree(t, lost.open(), map[string]string{"f": "foreign"})
}

func TestDamagedDatabaseWithoutCopyStopsTheStart(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "data")
	shutdown(t, e)
	for _, key := range f.bucket.keys(copyPrefix) {
		if err := f.bucket.remove(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.dir, databaseName), []byte(strings.Repeat("damaged!", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.tryOpen()
	if err == nil || !strings.Contains(err.Error(), "damaged and the bucket has no copy") {
		t.Fatalf("start = %v", err)
	}
}

// From #625: another server restores copy 3 and sends its start copy 5, but
// dies before it lands. The original folder comes back and also starts at 5,
// in a new timeline. When the late copy lands, the original still wins, from
// its own disk by history and from a new disk by commit count.
func TestLateCopyFromFailedTakeover(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "v1")
	copyNow(t, e)
	writeFile(t, e, "f", "v1 and a flush after the copy")
	folder := snapshot(t, e)
	kill(t, e)

	takeover := &fixture{t: t, bucket: f.bucket, dir: t.TempDir(), tune: testTuning()}
	takeover.tune.stopAge = 50 * time.Millisecond
	f.bucket.hold(func(op, key string) bool { return op == "put" && strings.HasPrefix(key, copyPrefix) })
	if _, err := takeover.tryOpen(); err == nil {
		t.Fatal("the takeover started without its start copy")
	}
	f.bucket.hold(nil)

	f.dir = folder
	e = f.open()
	if c := newestCopy(t, f.bucket); c.seq != 5 || c.history != historyOf(t, e) {
		t.Fatalf("start copy %+v", c)
	}
	f.bucket.release()
	want := map[string]string{"f": "v1 and a flush after the copy"}
	lost := &fixture{t: t, bucket: f.bucket.clone(), dir: t.TempDir(), tune: testTuning()}
	requireTree(t, lost.open(), want)
	// The late copy landed just above what this run saw at start, so this
	// timeline's trash does not protect its chunks: the v1 chunk was
	// trashed at sequence 3. Cleanup removes it before that chunk goes.
	for range 3 {
		copyNow(t, e)
	}
	checkCopies(t, f.bucket)
	kill(t, e)
	requireTree(t, f.open(), want)
}

func TestStartWaitsForItsCopy(t *testing.T) {
	f := newFixture(t)
	failures := 3
	f.bucket.setFault(func(op, key string) error {
		if op == "put" && strings.HasPrefix(key, copyPrefix) && failures > 0 {
			failures--
			return errors.New("injected copy failure")
		}
		return nil
	})
	e := f.open()
	if failures != 0 || len(f.bucket.keys(copyPrefix)) != 1 {
		t.Fatalf("failures left %d, copies %v", failures, f.bucket.keys(copyPrefix))
	}
	shutdown(t, e)
}

// A restart whose local database is ahead of the newest copy keeps it, and
// a crash anywhere in that start, also after the new timeline began, still
// keeps it on the next start.
func TestCrashDuringRestartKeepsFlushedData(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "in the copy")
	copyNow(t, e)
	writeFile(t, e, "f", "flushed after the copy")
	shutdown(t, e)
	ahead := map[string]string{"f": "flushed after the copy"}
	copied := map[string]string{"f": "in the copy"}
	fired := map[string]bool{}
	for at := 0; ; at++ {
		run := &fixture{t: t, bucket: f.bucket.clone(), dir: copyDir(t, f.dir), tune: testTuning()}
		c := &crasher{at: at}
		c.install(run)
		e, err := run.tryOpen()
		name := c.result()
		if name == "" {
			if err != nil {
				t.Fatal(err)
			}
			c.disarm()
			requireTree(t, e, ahead)
			break
		}
		fired[name] = true
		if e != nil {
			run.dir = snapshot(t, e)
			kill(t, e)
		}
		recoverAndCheck(t, run, name, []map[string]string{ahead}, []map[string]string{copied, ahead})
	}
	if !fired[stepStartedTimeline] {
		t.Error("never crashed after the new timeline began")
	}
}
