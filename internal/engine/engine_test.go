// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestFilesSurviveRestartAndDiskLoss(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "dir", smb.KindDirectory)
	writeFile(t, e, "dir/a", strings.Repeat("abcdefgh", 9))
	writeFile(t, e, "b", "short")
	want := map[string]string{"dir/": "", "dir/a": strings.Repeat("abcdefgh", 9), "b": "short"}
	if got := tree(t, e); !maps.Equal(got, want) {
		t.Fatalf("tree = %v", got)
	}
	shutdown(t, e)

	e = f.open()
	if got := tree(t, e); !maps.Equal(got, want) {
		t.Fatalf("after restart = %v", got)
	}
	copyNow(t, e)
	kill(t, e)

	// A new data folder is a lost disk: the newest copy comes back.
	f.dir = t.TempDir()
	e = f.open()
	if got := tree(t, e); !maps.Equal(got, want) {
		t.Fatalf("after disk loss = %v", got)
	}
	checkCopies(t, f.bucket)
}

func TestStartCopyIsHighestPlusTwo(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	copyNow(t, e)
	copyNow(t, e)
	shutdown(t, e)
	e = f.open()
	var seqs []int64
	for _, key := range f.bucket.keys(copyPrefix) {
		c, ok := parseCopy(key)
		if !ok {
			t.Fatal(key)
		}
		seqs = append(seqs, c.seq)
		if c.history == historyOf(t, e) && c.seq != 6 {
			t.Fatalf("start copy %s, want sequence 6", key)
		}
	}
	if len(seqs) != 4 || seqs[0] != 2 || seqs[3] != 6 {
		t.Fatalf("copies %v", seqs)
	}
}

func TestEarlyUploadsAndRewrites(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "big", smb.KindFile)
	h := openFile(t, e, "big", smb.AccessRead|smb.AccessWrite)
	data := []byte(strings.Repeat("0123456789abcdef", 10))
	for i := range data {
		writeAt(t, e, h, string(data[i]), uint64(i))
	}
	if n := len(f.bucket.keys(chunkPrefix)); n < 5 {
		t.Fatalf("only %d early uploads", n)
	}
	// Rewrite a chunk that went up early, then flush.
	writeAt(t, e, h, "XY", 3)
	copy(data[3:], "XY")
	if got := readHandle(t, e, h); got != string(data) {
		t.Fatalf("before flush = %q", got)
	}
	flush(t, e, h)
	closeFile(t, e, h)
	var pending, trash int
	if err := e.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM pending), (SELECT count(*) FROM trash)`).Scan(&pending, &trash); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || trash != 1 {
		t.Fatalf("pending %d, trash %d", pending, trash)
	}
	shutdown(t, e)
	e = f.open()
	if got := readFile(t, e, "big"); got != string(data) {
		t.Fatalf("after restart = %q", got)
	}
}

func TestPendingUploadsGoToTrashAtStart(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "big", smb.KindFile)
	h := openFile(t, e, "big", smb.AccessWrite)
	for i := range 6 {
		writeAt(t, e, h, strings.Repeat("x", 16), uint64(i*16))
	}
	dir := snapshot(t, e)
	kill(t, e)
	f.dir = dir
	e = f.open()
	var pending, trash int
	if err := e.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM pending), (SELECT count(*) FROM trash)`).Scan(&pending, &trash); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || trash != 2 {
		t.Fatalf("pending %d, trash %d", pending, trash)
	}
	if got := readFile(t, e, "big"); got != "" {
		t.Fatalf("unflushed data came back: %q", got)
	}
}

func TestTruncateZeroesTheTail(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", strings.Repeat("a", 40))
	h := openFile(t, e, "f", smb.AccessRead|smb.AccessWrite)
	if err := e.Truncate(t.Context(), h, 20); err != nil {
		t.Fatal(err)
	}
	if err := e.Truncate(t.Context(), h, 40); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("a", 20) + strings.Repeat("\x00", 20)
	if got := readHandle(t, e, h); got != want {
		t.Fatalf("read %q", got)
	}
	closeFile(t, e, h)
	shutdown(t, e)
	e = f.open()
	if got := readFile(t, e, "f"); got != want {
		t.Fatalf("after restart %q", got)
	}
}

func TestDamagedLocalDatabaseIsRestored(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	writeFile(t, e, "f", "kept")
	copyNow(t, e)
	writeFile(t, e, "g", "lost with the disk")
	shutdown(t, e)
	if err := os.WriteFile(filepath.Join(f.dir, databaseName), []byte(strings.Repeat("garbage!", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	e = f.open()
	if got := tree(t, e); !maps.Equal(got, map[string]string{"f": "kept"}) {
		t.Fatalf("tree = %v", got)
	}
}

// Chunks that uploaded before a FLUSH failed go to the trash and are deleted
// in time, rather than staying in S3 unknown.
func TestFailedFlushTrashesItsUploads(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	create(t, e, "f", smb.KindFile)
	h := openFile(t, e, "f", smb.AccessRead|smb.AccessWrite)
	writeAt(t, e, h, strings.Repeat("x", 48), 0)
	// The second upload fails as the client goes away, cancelling the FLUSH.
	ctx, cancel := context.WithCancel(t.Context())
	var puts atomic.Int32
	f.bucket.setFault(func(op, key string) error {
		if op == "put" && strings.HasPrefix(key, chunkPrefix) && puts.Add(1) == 2 {
			cancel()
			return context.Canceled
		}
		return nil
	})
	if err := e.Flush(ctx, h, smb.SyncData); err == nil {
		t.Fatal("the flush did not fail")
	}
	f.bucket.setFault(nil)
	flush(t, e, h)
	closeFile(t, e, h)
	for range 4 {
		copyNow(t, e)
	}
	if objects, rows := len(f.bucket.keys(chunkPrefix)), countRows(t, e, "chunks"); objects != rows {
		t.Fatalf("%d chunk objects for %d rows", objects, rows)
	}
	if got := readFile(t, e, "f"); got != strings.Repeat("x", 48) {
		t.Fatalf("read %q", got)
	}
}

// Concurrent writers to several files, some reading cold chunks first, stay
// within the RAM budget.
func TestConcurrentWritersKeepTheBudget(t *testing.T) {
	f := newFixture(t)
	e := f.open()
	const files = 4
	var handles [files]smb.Handle
	for i := range files {
		name := string(rune('a' + i))
		writeFile(t, e, name, strings.Repeat("x", 400))
		handles[i] = openFile(t, e, name, smb.AccessRead|smb.AccessWrite)
	}
	// Each write covers half a stored chunk, so it reads the chunk first.
	f.bucket.setFault(func(op, key string) error {
		if op == "get" && strings.HasPrefix(key, chunkPrefix) {
			time.Sleep(time.Millisecond)
		}
		return nil
	})
	var most atomic.Int64
	var group sync.WaitGroup
	for i := range files {
		group.Go(func() {
			for offset := uint64(0); offset < 400; offset += 8 {
				if _, err := e.WriteAt(t.Context(), handles[i], []byte("12345678"), offset); err != nil {
					t.Error(err)
					return
				}
				e.mu.Lock()
				n := int64(len(e.dirty))
				e.mu.Unlock()
				for old := most.Load(); n > old && !most.CompareAndSwap(old, n); old = most.Load() {
				}
			}
		})
	}
	group.Wait()
	if n := most.Load(); n > int64(f.tune.dirtyChunks) {
		t.Fatalf("%d dirty chunks with a budget of %d", n, f.tune.dirtyChunks)
	}
	for i := range files {
		flush(t, e, handles[i])
		if got := readHandle(t, e, handles[i]); got != strings.Repeat("12345678", 50) {
			t.Fatalf("file %d reads %q", i, got)
		}
	}
}

// A transaction that outlasts the lease does not commit.
func TestCommitRechecksTheLease(t *testing.T) {
	f := newFixture(t)
	f.tune.lease = time.Second
	e := f.open()
	e.timesMu.Lock()
	expiry := e.leaseUntil
	e.timesMu.Unlock()
	err := e.commit(t.Context(), func(tx *sql.Tx) error {
		time.Sleep(time.Until(expiry) + 10*time.Millisecond)
		_, execErr := tx.ExecContext(t.Context(), `INSERT INTO pending (name) VALUES ('late')`)
		return execErr
	})
	requireError(t, err, errLeaseExpired)
	var n int
	if err = e.db.QueryRowContext(t.Context(), `SELECT count(*) FROM pending`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d pending rows, %v", n, err)
	}
}
