// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"bytes"
	"maps"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

// The model keeps each path's content as reads see it (live), as SQLite has
// committed it (durable), and as the newest copy holds it (copied).
// Directories map to nil and end in a slash.
type model struct {
	t       testing.TB
	f       *fixture
	e       *Engine
	handles map[string]smb.Handle
	live    map[string][]byte
	durable map[string][]byte
	copied  map[string][]byte
}

var (
	modelFiles = []string{"a", "b", "d/c", "d/e"}
	modelDir   = "d/"
)

func newModel(t testing.TB) *model {
	m := &model{t: t, f: newFixture(t), handles: make(map[string]smb.Handle), live: map[string][]byte{}, durable: map[string][]byte{}}
	m.start()
	return m
}

// start opens the engine. Its start copy holds the durable state.
func (m *model) start() {
	m.e = m.f.open()
	m.live = clone(m.durable)
	m.copied = clone(m.durable)
	m.handles = make(map[string]smb.Handle)
}

func clone(state map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(state))
	for k, v := range state {
		out[k] = bytes.Clone(v)
	}
	return out
}

func parentExists(state map[string][]byte, path string) bool {
	_, ok := state[modelDir]
	return !strings.HasPrefix(path, modelDir) || ok
}

func (m *model) handle(path string) smb.Handle {
	if h := m.handles[path]; h != nil {
		return h
	}
	h := openFile(m.t, m.e, path, smb.AccessRead|smb.AccessWrite)
	m.handles[path] = h
	return h
}

// closeHandle closes an open handle, which flushes it.
func (m *model) closeHandle(path string) {
	if h := m.handles[path]; h != nil {
		closeFile(m.t, m.e, h)
		delete(m.handles, path)
		m.durable[path] = bytes.Clone(m.live[path])
	}
}

func resize(data []byte, size int) []byte {
	if size <= len(data) {
		return bytes.Clone(data[:size])
	}
	return append(bytes.Clone(data), make([]byte, size-len(data))...)
}

// step runs one operation chosen by op, with arguments from next.
func (m *model) step(op byte, next func() byte) {
	t := m.t
	path := modelFiles[int(next())%len(modelFiles)]
	_, exists := m.live[path]
	switch op % 14 {
	case 0: // create a file
		if !exists && parentExists(m.live, path) {
			create(t, m.e, path, smb.KindFile)
			m.live[path], m.durable[path] = []byte{}, []byte{}
		}
	case 1: // create the directory
		if _, ok := m.live[modelDir]; !ok {
			create(t, m.e, strings.TrimSuffix(modelDir, "/"), smb.KindDirectory)
			m.live[modelDir], m.durable[modelDir] = nil, nil
		}
	case 2, 3, 4:
		if exists {
			m.write(path, next)
		}
	case 5: // truncate
		if exists {
			size := next() % 120
			if err := m.e.Truncate(t.Context(), m.handle(path), uint64(size)); err != nil {
				t.Fatal(err)
			}
			m.live[path], m.durable[path] = resize(m.live[path], int(size)), resize(m.durable[path], int(size))
		}
	case 6:
		m.flush(path, next()%2 == 0)
	case 7: // close
		m.closeHandle(path)
	case 8: // read
		if exists {
			if got := readHandle(t, m.e, m.handle(path)); got != string(m.live[path]) {
				t.Fatalf("read %s = %q, want %q", path, got, m.live[path])
			}
		}
	case 9: // remove a file, or the empty directory
		m.remove(path, next()%4 == 0)
	case 10: // rename, replacing a file
		m.rename(path, modelFiles[int(next())%len(modelFiles)])
	case 11: // copy, which may also delete expired trash
		copyNow(t, m.e)
		m.copied = clone(m.durable)
		checkCopies(t, m.f.bucket)
	case 12:
		m.restart(next()%2 == 0)
	case 13: // crash and lose the disk
		kill(t, m.e)
		m.f.dir = t.TempDir()
		m.durable = clone(m.copied)
		m.start()
	}
}

func (m *model) write(path string, next func() byte) {
	offset, data := int(next()%100), make([]byte, 1+next()%40)
	for i := range data {
		data[i] = 'A' + next()%26
	}
	writeAt(m.t, m.e, m.handle(path), string(data), uint64(offset))
	content := m.live[path]
	if len(content) < offset+len(data) {
		content = resize(content, offset+len(data))
	}
	copy(content[offset:], data)
	m.live[path] = content
}

// flush flushes one file, or with full every file.
func (m *model) flush(path string, full bool) {
	h := m.handles[path]
	if h == nil {
		return
	}
	mode := smb.SyncData
	if full {
		mode = smb.SyncFull
	}
	if err := m.e.Flush(m.t.Context(), h, mode); err != nil {
		m.t.Fatal(err)
	}
	if full {
		for p := range m.handles {
			m.durable[p] = bytes.Clone(m.live[p])
		}
	}
	m.durable[path] = bytes.Clone(m.live[path])
}

// restart shuts down cleanly, or crashes and keeps the disk.
func (m *model) restart(clean bool) {
	if clean {
		for p := range m.handles {
			m.closeHandle(p)
		}
		shutdown(m.t, m.e)
	} else {
		m.f.dir = snapshot(m.t, m.e)
		kill(m.t, m.e)
	}
	m.start()
}

func (m *model) remove(path string, directory bool) {
	if directory {
		if _, ok := m.live[modelDir]; !ok {
			return
		}
		for p := range m.live {
			if p != modelDir && strings.HasPrefix(p, modelDir) {
				return
			}
		}
		path = modelDir
	} else if _, ok := m.live[path]; !ok {
		return
	}
	m.closeHandle(path)
	r := lookup(m.t, m.e, strings.TrimSuffix(path, "/"))
	if err := m.e.Remove(m.t.Context(), r.Name, r.Object.Inode); err != nil {
		m.t.Fatal(err)
	}
	delete(m.live, path)
	delete(m.durable, path)
}

func (m *model) rename(from, to string) {
	_, fromOK := m.live[from]
	if !fromOK || from == to || !parentExists(m.live, to) {
		return
	}
	m.closeHandle(from)
	m.closeHandle(to)
	source := lookup(m.t, m.e, from)
	destination, err := m.e.Lookup(m.t.Context(), to)
	if err != nil {
		m.t.Fatal(err)
	}
	err = m.e.Rename(m.t.Context(), smb.RenameRequest{
		Source: source.Name, SourceInode: source.Object.Inode,
		Destination: destination.Name, DestinationInode: destination.Object.Inode, Replace: true,
	})
	if err != nil {
		m.t.Fatal(err)
	}
	for _, state := range []map[string][]byte{m.live, m.durable} {
		state[to] = state[from]
		delete(state, from)
	}
}

// finish checks every path against the model, after a restart too.
func (m *model) finish() {
	if got := tree(m.t, m.e); !maps.EqualFunc(got, m.live, func(a string, b []byte) bool { return a == string(b) }) {
		m.t.Fatalf("tree = %q, want %q", got, m.live)
	}
	for p := range m.handles {
		m.closeHandle(p)
	}
	m.f.dir = snapshot(m.t, m.e)
	kill(m.t, m.e)
	m.start()
	if got := tree(m.t, m.e); !maps.EqualFunc(got, m.durable, func(a string, b []byte) bool { return a == string(b) }) {
		m.t.Fatalf("after restart tree = %q, want %q", got, m.durable)
	}
	checkCopies(m.t, m.f.bucket)
}

// runModel turns input into operations: one byte picks an operation and the
// following bytes give its arguments.
func runModel(t testing.TB, input []byte) {
	m := newModel(t)
	next := func() byte {
		if len(input) == 0 {
			return 0
		}
		b := input[0]
		input = input[1:]
		return b
	}
	for len(input) > 0 {
		m.step(next(), next)
	}
	m.finish()
}

func TestModel(t *testing.T) {
	for seed := range byte(40) {
		input := make([]byte, 600)
		if _, err := rand.NewChaCha8([32]byte{seed}).Read(input); err != nil {
			t.Fatal(err)
		}
		runModel(t, input)
	}
}

func FuzzModel(f *testing.F) {
	f.Add([]byte{0, 0, 2, 0, 3, 10, 8, 0, 6, 0, 0, 12, 0, 0, 8, 0})
	f.Add([]byte{1, 0, 0, 2, 2, 2, 50, 30, 11, 0, 5, 2, 20, 13, 0, 8, 2})
	f.Add([]byte{0, 1, 2, 1, 90, 39, 7, 1, 11, 0, 10, 1, 0, 11, 0, 11, 0, 11, 0, 11, 0, 13, 0})
	f.Fuzz(func(t *testing.T, input []byte) { runModel(t, input) })
}
