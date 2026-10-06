// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// TestCopiesTrashAndTakeoverUnderLoad runs copies every 250 ms and their trash
// cleanup while writers keep rewriting and deleting files, so the trash keeps
// filling. Meanwhile S3 loses replies and lands chunk PUTs and DELETEs late,
// and each round kills the engine in the middle of a copy or of a cleanup.
// After each run every kept copy, restored on its own on a new data folder,
// must hold what was flushed when it was captured; no copy may name a
// deleted chunk; and the trash must empty once the faults stop and copies
// move on. Then a second engine on a new data folder takes the bucket over
// and the same runs again.
func TestCopiesTrashAndTakeoverUnderLoad(t *testing.T) {
	rounds, length := 2, 2*time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		rounds, length = 8, 5*time.Second
	}
	seed, err := strconv.ParseUint(os.Getenv("S3_SMB_CHAOS_SEED"), 10, 64)
	if err != nil {
		seed = rand.Uint64() //nolint:gosec // Test faults, not secrets.
	}
	t.Logf("seed %d: replay the draws with S3_SMB_CHAOS_SEED=%d", seed, seed)
	m := &copyModel{files: map[string][]durable{}, captured: map[string]time.Time{}, seed: seed}
	bucket := newMemBucket()
	for server := range 2 {
		f := &fixture{t: t, bucket: bucket, dir: t.TempDir(), tune: testTuning()}
		f.tune.copyInterval = 250 * time.Millisecond
		f.tune.hook = m.hook(t, f.dir)
		e := f.open()
		if server == 1 {
			// The new server restored the newest copy; from now on the files
			// are what it holds.
			m.takeOver(tree(t, e))
		}
		for round := range rounds {
			target := []string{stepCopyCaptured, stepTrashDeleted}[round%2]
			stopFaults := faultyBucket(bucket, m.source(uint64(100*server+round)))
			armed := time.AfterFunc(length, func() { m.arm(target) })
			m.load(t, e, 2*length)
			armed.Stop()
			stopFaults()
			if !m.crashed(target) {
				t.Fatalf("server %d round %d: the engine was never killed at %q", server, round, target)
			}
			kill(t, e)
			e = f.open()
			m.reconcile(t, tree(t, e))
			// Before copies move on, the ones kept from the faulty run.
			m.checkKeptCopies(t, bucket)
		}
		drainTrash(t, e)
		m.checkKeptCopies(t, bucket)
		// The next server takes over from a crash.
		kill(t, e)
	}
}

// durable is a state of one file that an operation made durable between
// start and end. A zero end is an operation still running, or one cut off by
// a kill.
type durable struct {
	start, end time.Time
	content    string
	exists     bool
}

// copyModel follows what each file holds durably over time, the instant each
// copy was captured, and the kill it has to make.
type copyModel struct {
	killedAt time.Time
	files    map[string][]durable
	captured map[string]time.Time // by copy counter and history
	target   string               // the step to kill at, until it fires
	fired    string
	seed     uint64 // seeds the draws of the faults and the writers
	runs     uint64 // counts the load runs, so each draws anew
	mu       sync.Mutex
}

// source returns a source for one user of the draws.
func (m *copyModel) source(stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(m.seed, stream)) //nolint:gosec // Test faults, not secrets.
}

// hook records the instant of each capture, while no commit runs, and kills
// the engine once at the armed step.
func (m *copyModel) hook(t *testing.T, dir string) func(string) error {
	return func(step string) error {
		now := time.Now()
		if step == stepCopyCaptured {
			state, err := readFileState(context.Background(), filepath.Join(dir, copyTemp))
			if err != nil {
				t.Error(err)
			}
			m.mu.Lock()
			m.captured[captureKey(state.commits, state.history)] = now
			m.mu.Unlock()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if step != m.target {
			return nil
		}
		m.target, m.fired, m.killedAt = "", step, now
		return errCrash
	}
}

func captureKey(counter int64, history string) string { return fmt.Sprintf("%d-%s", counter, history) }

func (m *copyModel) arm(step string) {
	m.mu.Lock()
	m.target, m.fired = step, ""
	m.mu.Unlock()
}

func (m *copyModel) crashed(step string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fired == step
}

// faultyBucket loses one reply in ten after the request took effect, and
// holds one chunk PUT or DELETE in ten, or one copy DELETE, landing them
// late. The returned function stops the faults and lands what is held.
func faultyBucket(b *memBucket, rng *rand.Rand) func() {
	var mu sync.Mutex
	draw := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return rng.IntN(10) == 0
	}
	b.setLanded(func(op, _ string) error {
		if op != "get" && op != "list" && draw() {
			return errors.New("reply lost")
		}
		return nil
	})
	match := func(op, key string) bool {
		late := strings.HasPrefix(key, chunkPrefix) || op == "delete" && strings.HasPrefix(key, copyPrefix)
		return late && draw()
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			b.hold(match)
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			b.release()
		}
	}()
	return func() {
		close(stop)
		<-done
		b.setLanded(nil)
		b.release()
	}
}

// load runs one writer per file until the engine dies, or at most for
// length.
func (m *copyModel) load(t *testing.T, e *Engine, length time.Duration) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	m.runs++
	for w := range 8 {
		rng := m.source(1000*m.runs + uint64(w))
		wg.Go(func() { m.write(t, e, rng, fmt.Sprintf("f%d", w), stop) })
	}
	select {
	case <-time.After(length):
	case <-e.Dead():
	}
	close(stop)
	wg.Wait()
	// The kill may come after the writers stop; copies keep running.
	waitFor(t, "the armed kill", func() bool { return e.Err() != nil })
}

// write rewrites, creates and deletes one file, one operation at a time.
func (m *copyModel) write(t *testing.T, e *Engine, rng *rand.Rand, name string, stop <-chan struct{}) {
	ctx := context.Background()
	for v := 0; !stopped(stop) && e.Err() == nil; v++ {
		switch {
		case !m.current(name).exists:
			m.do(e, name, durable{exists: true}, func() error {
				r, err := e.Lookup(ctx, name)
				if err == nil {
					_, err = e.Create(ctx, r.Name, smb.KindFile)
				}
				return err
			})
		case rng.IntN(4) == 0:
			m.do(e, name, durable{}, func() error {
				r, err := e.Lookup(ctx, name)
				if err == nil {
					err = e.Remove(ctx, r.Name, r.Object)
				}
				return err
			})
		default:
			m.rewrite(t, e, name, strings.Repeat(fmt.Sprintf("%s v%05d ", name, v), 8)[:80])
		}
	}
}

// rewrite writes content over the file and flushes it. Every version has the
// same length, so a rewrite replaces each chunk.
func (m *copyModel) rewrite(t *testing.T, e *Engine, name, content string) {
	ctx := context.Background()
	r, err := e.Lookup(ctx, name)
	if err != nil {
		return
	}
	h, err := e.Open(ctx, r.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		return
	}
	defer func() {
		if closeErr := e.Close(ctx, h); closeErr != nil && e.Err() == nil {
			t.Error(closeErr)
		}
	}()
	// Ten bytes at a time pass the RAM budget and upload chunks early.
	for i := 0; i < len(content); {
		if e.Err() != nil {
			return
		}
		if _, err = e.WriteAt(ctx, h, []byte(content[i:i+10]), uint64(i)); err == nil {
			i += 10
		}
	}
	m.do(e, name, durable{exists: true, content: content}, func() error { return e.Flush(ctx, h, smb.SyncData) })
}

func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// do records state as running, retries op until it succeeds or the engine
// dies, and records when it succeeded.
func (m *copyModel) do(e *Engine, name string, state durable, op func() error) {
	m.mu.Lock()
	state.start = time.Now()
	m.files[name] = append(m.files[name], state)
	i := len(m.files[name]) - 1
	m.mu.Unlock()
	for op() != nil {
		if e.Err() != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	m.mu.Lock()
	m.files[name][i].end = time.Now()
	m.mu.Unlock()
}

// current returns the last state of a file, done or not.
func (m *copyModel) current(name string) durable {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := m.files[name]
	if len(states) == 0 {
		return durable{}
	}
	return states[len(states)-1]
}

// allowed returns the states a file may be in for a copy captured at: the
// last one made durable before it, or one whose operation was still running.
func (m *copyModel) allowed(name string, at time.Time) []durable {
	last := durable{}
	var running []durable
	for _, d := range m.files[name] {
		switch {
		case !d.end.IsZero() && d.end.Before(at):
			last = d
		case d.start.Before(at):
			running = append(running, d)
		}
	}
	return append(running, last)
}

// reconcile settles the operations a kill cut off, by what the same data
// folder holds after the restart: each one either committed before the kill
// or never did.
func (m *copyModel) reconcile(t *testing.T, got map[string]string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, states := range m.files {
		if !matches(states[len(states)-1], name, got) && states[len(states)-1].end.IsZero() {
			states = states[:len(states)-1]
		}
		if len(states) > 0 && states[len(states)-1].end.IsZero() {
			states[len(states)-1].end = m.killedAt
		}
		m.files[name] = states
		if want := m.lastDone(name); !matches(want, name, got) {
			t.Fatalf("after a kill, the same data folder holds %s as %q (present %v), want %q (present %v)", name, got[name], hasKey(got, name), want.content, want.exists)
		}
	}
}

func (m *copyModel) lastDone(name string) durable {
	states := m.files[name]
	if len(states) == 0 {
		return durable{}
	}
	return states[len(states)-1]
}

// takeOver makes the files what a new server restored, from now on.
func (m *copyModel) takeOver(got map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for name := range m.files {
		content, ok := got[name]
		m.files[name] = append(m.files[name], durable{start: now, end: now, content: content, exists: ok})
	}
}

func matches(d durable, name string, got map[string]string) bool {
	content, ok := got[name]
	return ok == d.exists && (!ok || content == d.content)
}

func hasKey(got map[string]string, name string) bool {
	_, ok := got[name]
	return ok
}

// drainTrash makes copies, with no faults, until the trash is empty.
func drainTrash(t *testing.T, e *Engine) {
	t.Helper()
	waitFor(t, "the trash to empty", func() bool {
		if err := e.makeCopy(t.Context(), 0); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := e.db.QueryRowContext(t.Context(), `SELECT count(*) FROM trash`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	})
}

// checkKeptCopies takes a snapshot of the bucket, while copies go on, and
// requires every chunk its copies name. Then it restores each copy on its
// own, on a new data folder from the snapshot without the newer copies, and
// checks every file against the states allowed when that copy was captured.
func (m *copyModel) checkKeptCopies(t *testing.T, bucket *memBucket) {
	t.Helper()
	snapshot := bucket.clone()
	checkCopies(t, snapshot)
	keys := snapshot.keys(copyPrefix)
	if len(keys) < testTuning().copiesKept {
		t.Fatalf("only %d copies kept: %q", len(keys), keys)
	}
	for _, key := range keys {
		c, ok := parseCopy(key)
		if !ok {
			continue
		}
		m.mu.Lock()
		at, recorded := m.captured[captureKey(c.counter, c.history)]
		m.mu.Unlock()
		if !recorded {
			t.Fatalf("copy %s has no recorded capture", key)
		}
		clone := snapshot.clone()
		for _, other := range keys {
			if n, ok := parseCopy(other); ok && n.seq > c.seq {
				delete(clone.objects, other)
			}
		}
		restored := &fixture{t: t, bucket: clone, dir: t.TempDir(), tune: testTuning()}
		e := restored.open()
		got := tree(t, e)
		kill(t, e)
		m.mu.Lock()
		for name := range m.files {
			allowed := m.allowed(name, at)
			if !slices.ContainsFunc(allowed, func(d durable) bool { return matches(d, name, got) }) {
				m.mu.Unlock()
				t.Fatalf("copy %s restores %s as %q (present %v); allowed %+v", key, name, got[name], hasKey(got, name), allowed)
			}
		}
		for name := range got {
			if _, ok := m.files[name]; !ok {
				m.mu.Unlock()
				t.Fatalf("copy %s restores %s, which was never written", key, name)
			}
		}
		m.mu.Unlock()
	}
	t.Logf("each of %d kept copies restored what was flushed when it was captured", len(keys))
}
