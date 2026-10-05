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
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Temporary for #598: a larger tree, a first backup and several incremental
// backups, so the SMB trace shows how often the same band chunks are rewritten.

type hotspotChange struct {
	Backup        string    `json:"backup"`
	Description   string    `json:"description"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	Selected      string    `json:"selected"`
	AddedFiles    int       `json:"added_files"`
	AddedBytes    int64     `json:"added_bytes"`
	ChangedFiles  int       `json:"changed_files"`
	ChangedBytes  int64     `json:"changed_bytes"`
	WrittenBytes  int64     `json:"written_bytes"`
	DeletedFiles  int       `json:"deleted_files"`
	DeletedBytes  int64     `json:"deleted_bytes"`
	TreeFiles     int       `json:"tree_files"`
	TreeBytes     int64     `json:"tree_bytes"`
	PrepareStart  time.Time `json:"prepare_start"`
	PrepareFinish time.Time `json:"prepare_finish"`
}

type hotspotTree struct {
	h     *harness
	rng   *mathrand.Rand
	files map[string]int64
	next  int
	large bool
}

func (h *harness) newHotspotTree() *hotspotTree {
	return &hotspotTree{h: h, rng: mathrand.New(mathrand.NewPCG(598, 1)), files: map[string]int64{}}
}

func topDir(name string) string {
	return strings.SplitN(name, "/", 2)[0]
}

// bigTree adds about 4.4 GiB of random data in files of mixed sizes.
func (h *harness) bigTree() *hotspotTree {
	tree := h.newHotspotTree()
	for _, group := range []struct {
		dir   string
		count int
		size  int64
	}{
		{"large", 3, 1 << 30},
		{"medium", 8, 128 << 20},
		{"photos", 100, 8 << 20},
		{"docs", 400, 1 << 20},
		{"code", 2000, 64 << 10},
		{"notes", 20000, 4 << 10},
	} {
		for i := range group.count {
			tree.add(fmt.Sprintf("%s/d%03d/f%05d.bin", group.dir, i%100, i), group.size)
		}
	}
	return tree
}

func (t *hotspotTree) add(name string, size int64) {
	h := t.h
	h.must(h.proofDir.MkdirAll(path.Dir(name), 0o700))
	file, err := h.proofDir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	h.must(err)
	_, err = io.CopyN(file, rand.Reader, size)
	h.must(errors.Join(err, file.Close()))
	t.files[name] = size
}

// overwrite writes length random bytes at a random offset, in place.
func (t *hotspotTree) overwrite(name string, length int64) {
	h := t.h
	size := t.files[name]
	length = min(length, size)
	offset := t.rng.Int64N(size - length + 1)
	file, err := h.proofDir.OpenFile(name, os.O_WRONLY, 0)
	h.must(err)
	_, err = file.Seek(offset, io.SeekStart)
	h.must(err)
	_, err = io.CopyN(file, rand.Reader, length)
	h.must(errors.Join(err, file.Close()))
}

func (t *hotspotTree) appendTo(name string, length int64) {
	h := t.h
	file, err := h.proofDir.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
	h.must(err)
	_, err = io.CopyN(file, rand.Reader, length)
	h.must(errors.Join(err, file.Close()))
	t.files[name] += length
}

func (t *hotspotTree) remove(name string) {
	t.h.must(t.h.proofDir.Remove(name))
	delete(t.files, name)
}

// pick returns n distinct random files under dir, in a stable order.
func (t *hotspotTree) pick(dir string, n int) []string {
	var names []string
	for name := range t.files {
		if dir == "" || topDir(name) == dir {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	t.rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	return names[:min(n, len(names))]
}

func (t *hotspotTree) totals() (int, int64) {
	var total int64
	for _, size := range t.files {
		total += size
	}
	return len(t.files), total
}

// change applies one step of edits and returns what it did.
func (t *hotspotTree) change(step int) hotspotChange {
	c := hotspotChange{PrepareStart: time.Now().UTC()}
	addN := func(dir string, n int, size int64) {
		for range n {
			t.next++
			t.add(fmt.Sprintf("%s/new/s%d-%05d.bin", dir, step, t.next), size)
			c.AddedFiles++
			c.AddedBytes += size
			c.WrittenBytes += size
		}
	}
	edit := func(dir string, n int, length int64) {
		for _, name := range t.pick(dir, n) {
			t.overwrite(name, length)
			c.ChangedFiles++
			c.ChangedBytes += t.files[name]
			c.WrittenBytes += min(length, t.files[name])
		}
	}
	grow := func(dir string, n int, length int64) {
		for _, name := range t.pick(dir, n) {
			t.appendTo(name, length)
			c.ChangedFiles++
			c.ChangedBytes += t.files[name]
			c.WrittenBytes += length
		}
	}
	drop := func(dir string, n int) {
		for _, name := range t.pick(dir, n) {
			c.DeletedFiles++
			c.DeletedBytes += t.files[name]
			t.remove(name)
		}
	}
	if t.large {
		t.largeChange(step, &c, addN, edit, drop)
		c.TreeFiles, c.TreeBytes = t.totals()
		c.PrepareFinish = time.Now().UTC()
		return c
	}
	switch step {
	case 1:
		c.Description = "typical: edit 2% of notes and code, grow 5 photos, add 100 code files and one 128 MiB file, delete 1% of notes and 2 photos"
		edit("notes", 400, 4<<10)
		edit("code", 40, 16<<10)
		grow("photos", 5, 1<<20)
		addN("code", 100, 64<<10)
		addN("medium", 1, 128<<20)
		drop("notes", 200)
		drop("photos", 2)
	case 2:
		c.Description = "large: overwrite 16 MiB inside one 1 GiB file, add 500 MiB of new files, delete one 128 MiB file"
		edit("large", 1, 16<<20)
		addN("photos", 50, 8<<20)
		addN("docs", 100, 1<<20)
		drop("medium", 1)
	case 3:
		c.Description = "typical: edit 2% of notes and code and 10 docs, add 20 photos, delete 1% of notes"
		edit("notes", 400, 4<<10)
		edit("code", 40, 16<<10)
		edit("docs", 10, 64<<10)
		addN("photos", 20, 8<<20)
		drop("notes", 200)
	case 4:
		c.Description = "delete-heavy: delete one 1 GiB file, 20% of notes and 10% of code, add 50 MiB"
		drop("large", 1)
		drop("notes", 4000)
		drop("code", 200)
		addN("docs", 50, 1<<20)
	case 5:
		c.Description = "tiny: edit 10 notes"
		edit("notes", 10, 4<<10)
	default:
		t.h.t.Fatal("unknown hotspot step", step)
	}
	c.TreeFiles, c.TreeBytes = t.totals()
	c.PrepareFinish = time.Now().UTC()
	return c
}

// hotspotScenario runs the first backup of the big tree, then five changed
// incremental backups back to back. Every backup is listed, not restored.
func (h *harness) hotspotScenario() result {
	large := h.hotspots != nil && h.hotspots.large
	start := time.Now().UTC()
	outcome := h.baseline()
	files, bytes := h.hotspots.totals()
	changes := []hotspotChange{{Backup: "baseline", Description: "first backup", Start: h.hotspotStart, End: time.Now().UTC(), Selected: outcome.Baseline, AddedFiles: files, AddedBytes: bytes, TreeFiles: files, TreeBytes: bytes, PrepareStart: start}}
	previous := outcome.Baseline
	for step := 1; step <= 5; step++ {
		label := fmt.Sprintf("inc%d", step)
		if large {
			h.hotspotDisk("before-" + label)
			if free := h.freeBytes(); free < 45<<30 {
				h.t.Log("hotspots-stop-low-disk", label, free)
				break
			}
		}
		change := h.hotspots.change(step)
		change.Backup = label
		h.t.Log("hotspots-change", label, change.Description)
		change.Start = time.Now().UTC()
		h.t.Log("hotspots-backup-start", label, change.Start.Format(time.RFC3339Nano))
		h.startBackup(label)
		_, err := h.completeBackup(label)
		h.must(err)
		h.t.Log("hotspots-backup-end", label, time.Now().UTC().Format(time.RFC3339Nano))
		h.must(h.detach())
		h.mount()
		latest := filepath.Base(h.remoteBackup(label, ""))
		h.must(h.detach())
		change.End = time.Now().UTC()
		change.Selected = latest
		if latest == previous {
			h.t.Fatal("incremental backup did not produce a new backup", label)
		}
		previous = latest
		changes = append(changes, change)
		h.save("hotspots-changes-"+label+".json", changes)
	}
	h.save("hotspots-changes.json", changes)
	if large {
		h.hotspotDisk("end")
	}
	outcome.Scenario, outcome.Resumed = "hotspots", previous
	return outcome
}
