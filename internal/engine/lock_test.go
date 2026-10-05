// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func lockKeys(b *memBucket) []string { return b.keys(lockPrefix) }

func TestSameFolderTakesOverAtOnce(t *testing.T) {
	f := newFixture(t)
	f.tune.stale = time.Hour
	kill(t, f.open())
	start := time.Now()
	shutdown(t, f.open())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s", elapsed)
	}
	if keys := lockKeys(f.bucket); len(keys) != 1 {
		t.Fatalf("lock keys %v: the dead run's key stays, the clean one goes", keys)
	}
}

func TestNewFolderWaitsUntilTheLockIsStale(t *testing.T) {
	f := newFixture(t)
	f.tune.stale = 300 * time.Millisecond
	kill(t, f.open())
	f.dir = t.TempDir()
	start := time.Now()
	e := f.open()
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("took over after %s", elapsed)
	}
	shutdown(t, e)
}

func TestCleanExitLetsAnotherServerStart(t *testing.T) {
	f := newFixture(t)
	f.tune.stale = time.Hour
	shutdown(t, f.open())
	if keys := lockKeys(f.bucket); len(keys) != 0 {
		t.Fatalf("lock keys %v", keys)
	}
	f.dir = t.TempDir()
	shutdown(t, f.open())
}

// Two servers that start at once: one serves, the other waits until the
// first exits.
func TestTwoServersStartingAtOnce(t *testing.T) {
	bucket := newMemBucket()
	opened := make(chan *Engine, 2)
	var serving atomic.Int32
	for range 2 {
		f := &fixture{t: t, bucket: bucket, dir: t.TempDir(), tune: testTuning()}
		f.tune.stale = time.Hour
		go func() {
			e, err := open(t.Context(), Options{Dir: f.dir}, bucket, f.tune)
			if err != nil {
				t.Error(err)
				opened <- nil
				return
			}
			if serving.Add(1) > 1 {
				t.Error("two servers serve at once")
			}
			opened <- e
		}()
	}
	first := <-opened
	if first == nil {
		t.FailNow()
	}
	select {
	case e := <-opened:
		t.Fatal("the second server started while the first serves", e != nil)
	case <-time.After(300 * time.Millisecond):
	}
	serving.Add(-1)
	shutdown(t, first)
	second := <-opened
	if second == nil {
		t.FailNow()
	}
	shutdown(t, second)
}

func TestLeaseExpiryIsFinal(t *testing.T) {
	f := newFixture(t)
	f.tune.renewEvery = 10 * time.Millisecond
	f.tune.lease = 100 * time.Millisecond
	e := f.open()
	writeFile(t, e, "f", "before")
	h := openFile(t, e, "f", smb.AccessWrite)
	f.bucket.setFault(func(op, key string) error {
		if strings.HasPrefix(key, lockPrefix) {
			return errors.New("injected lock failure")
		}
		return nil
	})
	select {
	case <-e.Dead():
	case <-time.After(10 * time.Second):
		t.Fatal("the engine outlived its lease")
	}
	requireError(t, e.Err(), errLeaseExpired)
	f.bucket.setFault(nil)
	writeAt(t, e, h, "after", 0)
	requireError(t, e.Flush(t.Context(), h, smb.SyncData), smb.ErrIO)
	_, err := e.Create(t.Context(), smb.Name{Parent: rootInode, Base: "g"}, smb.KindFile)
	requireError(t, err, errLeaseExpired)
	if keys := f.bucket.keys(chunkPrefix); len(keys) != 1 {
		t.Fatalf("chunks after expiry: %v", keys)
	}
}

// A 5-minute outage that starts just after a renewal only slows things down,
// here scaled down by 600.
func TestOutageJustAfterRenewal(t *testing.T) {
	f := newFixture(t)
	f.tune.renewEvery = 100 * time.Millisecond
	f.tune.lease = 800 * time.Millisecond
	f.tune.stale = time.Second
	renewed := make(chan struct{}, 1)
	f.bucket.setLanded(func(op, key string) error {
		if op == "put" && strings.HasPrefix(key, lockPrefix) {
			select {
			case renewed <- struct{}{}:
			default:
			}
		}
		return nil
	})
	e := f.open()
	<-renewed
	<-renewed
	f.bucket.setFault(func(string, string) error { return errors.New("injected outage") })
	time.Sleep(500 * time.Millisecond)
	f.bucket.setFault(nil)
	writeFile(t, e, "f", "after the outage")
	time.Sleep(time.Second)
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	shutdown(t, e)
}

// A server that freezes loses the bucket once its lock is stale. When it
// resumes, its lease has expired, so it writes nothing more. Requests it had
// already checked land late and do no harm.
func TestFrozenServerResumes(t *testing.T) {
	bucket := newMemBucket()
	tune := testTuning()
	tune.renewEvery = 20 * time.Millisecond
	tune.lease = 160 * time.Millisecond
	tune.stale = 200 * time.Millisecond
	view := &pausable{objects: bucket}
	a, err := open(t.Context(), Options{Dir: t.TempDir()}, view, tune)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kill(t, a) })
	writeFile(t, a, "f", "from the old server")
	copyNow(t, a)
	h := openFile(t, a, "f", smb.AccessWrite)
	writeAt(t, a, h, "unflushed", 0)

	view.gate.Lock()
	next := &fixture{t: t, bucket: bucket, dir: t.TempDir(), tune: tune}
	b := next.open()
	writeFile(t, b, "g", "from the new server")
	view.gate.Unlock()

	requireError(t, a.Flush(t.Context(), h, smb.SyncData), smb.ErrIO)
	requireError(t, a.Err(), errLeaseExpired)
	copyNow(t, b)
	requireTree(t, b, map[string]string{"f": "from the old server", "g": "from the new server"})
	checkCopies(t, bucket)
}
