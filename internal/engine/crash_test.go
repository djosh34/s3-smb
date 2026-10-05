// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

var errCrash = errors.New("crash")

// crasher counts points: every local step and every S3 request once it has
// taken effect. At point number at it fires, and from then on every point
// and request fails, as if the process were gone.
type crasher struct {
	fired string
	at    int
	mu    sync.Mutex
}

func (c *crasher) point(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fired != "" {
		return errCrash
	}
	if c.at == 0 {
		c.fired = name
		return errCrash
	}
	c.at--
	return nil
}

func (c *crasher) install(f *fixture) {
	f.tune.hook = c.point
	f.bucket.setFault(func(string, string) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.fired != "" {
			return errCrash
		}
		return nil
	})
	f.bucket.setLanded(func(op, key string) error { return c.point(op + " " + strings.SplitN(key, "/", 2)[0]) })
}

func (c *crasher) result() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

// steps is a run of file operations that each return an error instead of
// failing the test, since a crash makes them fail.
type steps struct {
	e     *Engine
	state map[string]string
}

func (s *steps) resolve(path string) (smb.Resolved, error) { return s.e.Lookup(context.Background(), path) }

func (s *steps) create(path string, kind smb.Kind) error {
	r, err := s.resolve(path)
	if err == nil {
		_, err = s.e.Create(context.Background(), r.Name, kind)
	}
	if err == nil {
		if kind == smb.KindDirectory {
			s.state[path+"/"] = ""
		} else {
			s.state[path] = ""
		}
	}
	return err
}

func (s *steps) write(path, data string, offset int) error {
	ctx := context.Background()
	r, err := s.resolve(path)
	if err != nil {
		return err
	}
	h, err := s.e.Open(ctx, r.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		return err
	}
	// Ten bytes at a time, so a long write passes the RAM budget and uploads
	// chunks early.
	for i := 0; i < len(data) && err == nil; i += 10 {
		_, err = s.e.WriteAt(ctx, h, []byte(data[i:min(i+10, len(data))]), uint64(offset+i))
	}
	if err == nil {
		err = s.e.Flush(ctx, h, smb.SyncData)
	}
	err = errors.Join(err, s.e.Close(ctx, h))
	if err == nil {
		content := []byte(s.state[path])
		content = append(content, make([]byte, max(0, offset+len(data)-len(content)))...)
		copy(content[offset:], data)
		s.state[path] = string(content)
	}
	return err
}

func (s *steps) truncate(path string, size uint64) error {
	r, err := s.resolve(path)
	if err == nil {
		err = s.e.SetAttr(context.Background(), r.Object, smb.AttrChange{Size: &size})
	}
	if err == nil {
		s.state[path] = s.state[path][:size]
	}
	return err
}

func (s *steps) rename(from, to string) error {
	source, err := s.resolve(from)
	if err != nil {
		return err
	}
	destination, err := s.resolve(to)
	if err == nil {
		err = s.e.Rename(context.Background(), smb.RenameRequest{Source: source.Name, SourceInode: source.Object.Inode, Destination: destination.Name})
	}
	if err == nil {
		s.state[to] = s.state[from]
		delete(s.state, from)
	}
	return err
}

func (s *steps) remove(path string) error {
	r, err := s.resolve(path)
	if err == nil {
		err = s.e.Remove(context.Background(), r.Name, r.Object.Inode)
	}
	if err == nil {
		delete(s.state, path)
	}
	return err
}

func (s *steps) copies(n int) error {
	for range n {
		if err := s.e.makeCopy(context.Background(), 0); err != nil {
			return err
		}
	}
	return nil
}

// crashScenario records the state after each step. The 70-byte write uploads
// chunks early, the rewrite and truncate trash chunks, and the copies at the
// end delete them.
func crashScenario(e *Engine, record func(map[string]string)) error {
	s := &steps{e: e, state: map[string]string{}}
	for _, step := range []func() error{
		func() error { return s.create("d", smb.KindDirectory) },
		func() error { return s.create("d/a", smb.KindFile) },
		func() error { return s.write("d/a", strings.Repeat("0123456789", 7), 0) },
		func() error { return s.copies(1) },
		func() error { return s.write("d/a", "REWRITE", 30) },
		func() error { return s.truncate("d/a", 20) },
		func() error { return s.rename("d/a", "b") },
		func() error { return s.create("c", smb.KindFile) },
		func() error { return s.write("c", strings.Repeat("c", 40), 2) },
		func() error { return s.copies(4) },
		func() error { return s.remove("b") },
		func() error { return s.copies(5) },
	} {
		if err := step(); err != nil {
			return err
		}
		record(maps.Clone(s.state))
	}
	return nil
}

func TestCrashAtEveryPoint(t *testing.T) {
	reference := newFixture(t)
	states := []map[string]string{{}}
	if err := crashScenario(reference.open(), func(s map[string]string) { states = append(states, s) }); err != nil {
		t.Fatal(err)
	}
	fired := map[string]bool{}
	for at := 0; ; at++ {
		f := newFixture(t)
		c := &crasher{at: at}
		c.install(f)
		done := 0
		e, err := f.tryOpen()
		if err == nil {
			err = crashScenario(e, func(map[string]string) { done++ })
		}
		name := c.result()
		if name == "" {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		fired[name] = true
		if e != nil {
			f.dir = snapshot(t, e)
			kill(t, e)
		}
		// The step in flight may have committed before its reply was lost.
		last := min(done+1, len(states)-1)
		recoverAndCheck(t, f, name, states[done:last+1], states[:last+1])
	}
	for _, step := range []string{stepCommit, stepFlushReply, stepPending, stepCopyAttempt, stepCopyCaptured, stepTrashDeleted, "put chunks", "put db", "delete chunks", "delete db"} {
		if !fired[step] {
			t.Errorf("never crashed at %q; crashed at %v", step, slices.Sorted(maps.Keys(fired)))
		}
	}
}

// recoverAndCheck restarts after a crash. From the kept disk the files must
// match one of sameDisk, and from a new disk one of anyCopy. Every copy left
// in the bucket must still have all its chunks.
func recoverAndCheck(t *testing.T, f *fixture, name string, sameDisk, anyCopy []map[string]string) {
	t.Helper()
	lost := &fixture{t: t, bucket: f.bucket.clone(), dir: t.TempDir(), tune: testTuning()}
	f.bucket.setFault(nil)
	f.bucket.setLanded(nil)
	f.tune = testTuning()
	for _, check := range []struct {
		f       *fixture
		allowed []map[string]string
	}{{f, sameDisk}, {lost, anyCopy}} {
		e, err := check.f.tryOpen()
		if err != nil {
			t.Fatalf("crash at %q: restart: %v", name, err)
		}
		got := tree(t, e)
		if !slices.ContainsFunc(check.allowed, func(s map[string]string) bool { return maps.Equal(s, got) }) {
			t.Fatalf("crash at %q: tree %q, want one of %q", name, got, check.allowed)
		}
		checkCopies(t, check.f.bucket)
		shutdown(t, e)
	}
}
