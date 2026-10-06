//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// large backs up the medium tree of the second hot spot measurement (#598),
// about 7 GiB in 36,000 files, then three incrementals that each edit 10% to
// 30% of all files, add up to 2 GiB and delete up to 10%. The latest backup
// must restore.
func (h *harness) large() result {
	tree := &largeTree{h: h, rng: mathrand.New(mathrand.NewPCG(620, 1)), files: map[string]int64{}} //nolint:gosec // The tree only needs a replayable shape.
	h.prepare = tree.create
	outcome := h.baseline()
	outcome.Scenario = "large"
	latest := outcome.Baseline
	var updated []helpers.Entry
	for step, change := range []largeChange{{edit: 0.10, add: 1 << 30, drop: 0.02}, {edit: 0.30, add: 2 << 30, drop: 0.05}, {edit: 0.20, add: 512 << 20, drop: 0.10}} {
		label := fmt.Sprintf("large-%d", step+1)
		latest, updated = h.incremental(label, latest, func() { tree.change(label, change) })
	}
	outcome.Resumed = latest
	outcome.ResumedRestore = h.restoreBackup(latest, updated, "restore-latest")
	return outcome
}

// largeTree is the test tree of the large scenario: file paths and sizes.
type largeTree struct {
	h     *harness
	rng   *mathrand.Rand
	files map[string]int64
}

// largeGroups are the tree's file groups at a quarter of the #598 large tree.
var largeGroups = []struct {
	dir   string
	count int
	size  int64
}{
	{"large", 2, 1 << 30},
	{"medium", 12, 128 << 20},
	{"photos", 200, 8 << 20},
	{"docs", 1250, 1 << 20},
	{"code", 10000, 64 << 10},
	{"notes", 25000, 4 << 10},
}

func (t *largeTree) create() {
	for _, group := range largeGroups {
		for i := range group.count {
			t.write(fmt.Sprintf("tree/%s/d%03d/f%06d.bin", group.dir, i%100, i), group.size)
		}
	}
}

// largeChange edits a share of every group's files in place, adds new 8 MiB
// files and deletes a share of every group.
type largeChange struct {
	edit, drop float64
	add        int64
}

func (t *largeTree) change(label string, c largeChange) {
	for _, group := range largeGroups {
		// Large files get 1 MiB edits, the rest 4 KiB.
		length := int64(4 << 10)
		if group.size >= 128<<20 {
			length = 1 << 20
		}
		names := t.pick(group.dir)
		for _, name := range names[:int(float64(len(names))*c.edit+0.5)] {
			t.overwrite(name, length)
		}
		names = t.pick(group.dir)
		for _, name := range names[:int(float64(len(names))*c.drop+0.5)] {
			t.h.must(t.h.proofDir.Remove(name))
			delete(t.files, name)
		}
	}
	for i := range c.add / (8 << 20) {
		t.write(fmt.Sprintf("tree/added/%s/f%04d.bin", label, i), 8<<20)
	}
}

func (t *largeTree) write(name string, size int64) {
	h := t.h
	h.must(h.proofDir.MkdirAll(path.Dir(name), 0o700))
	file, err := h.proofDir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	h.must(err)
	_, err = io.CopyN(file, rand.Reader, size)
	h.must(errors.Join(err, file.Close()))
	t.files[name] = size
}

// overwrite writes length random bytes at a random offset, in place.
func (t *largeTree) overwrite(name string, length int64) {
	h := t.h
	length = min(length, t.files[name])
	file, err := h.proofDir.OpenFile(name, os.O_WRONLY, 0)
	h.must(err)
	_, err = file.Seek(t.rng.Int64N(t.files[name]-length+1), io.SeekStart)
	if err == nil {
		_, err = io.CopyN(file, rand.Reader, length)
	}
	h.must(errors.Join(err, file.Close()))
}

// pick returns the group's files in a random but replayable order.
func (t *largeTree) pick(dir string) []string {
	var names []string
	for name := range t.files {
		if strings.HasPrefix(name, "tree/"+dir+"/") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	t.rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	return names
}
