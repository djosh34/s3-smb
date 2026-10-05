//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"fmt"
	"syscall"
	"time"
)

// Temporary for #598, second run: a tree sized from free disk, and
// incrementals that edit 10% to 30% of all files.

func (h *harness) freeBytes() int64 {
	var stat syscall.Statfs_t
	h.must(syscall.Statfs(h.work, &stat))
	return int64(stat.Bavail) * int64(stat.Bsize)
}

// hotspotDisk logs where the disk space goes.
func (h *harness) hotspotDisk(label string) {
	output, err := h.try(3*time.Minute, "/bin/sh", "-c", "/usr/bin/du -sk "+h.work+"/objects "+h.local+" 2>&1; /usr/bin/tmutil listlocalsnapshots / 2>&1 | /usr/bin/wc -l")
	h.t.Log("hotspots-disk", label, "free_bytes", h.freeBytes(), output, err)
}

// largeTree fills about scale times 28 GiB, in 145,856 files at scale 1.
func (h *harness) largeTree(scale float64) *hotspotTree {
	tree := h.newHotspotTree()
	tree.large = true
	for _, group := range []struct {
		dir   string
		count int
		size  int64
	}{
		{"large", 8, 1 << 30},
		{"medium", 48, 128 << 20},
		{"photos", 800, 8 << 20},
		{"docs", 5000, 1 << 20},
		{"code", 40000, 64 << 10},
		{"notes", 100000, 4 << 10},
	} {
		n := max(1, int(float64(group.count)*scale))
		for i := range n {
			tree.add(fmt.Sprintf("%s/d%03d/f%06d.bin", group.dir, i%200, i), group.size)
		}
	}
	return tree
}

var largeGroups = []string{"large", "medium", "photos", "docs", "code", "notes"}

// largeChange edits, adds and deletes a share of every group.
func (t *hotspotTree) largeChange(step int, c *hotspotChange, addN func(string, int, int64), edit func(string, int, int64), drop func(string, int)) {
	count := func(dir string) int {
		n := 0
		for name := range t.files {
			if topDir(name) == dir {
				n++
			}
		}
		return n
	}
	editShare := func(share float64, small, big int64) {
		for _, dir := range largeGroups {
			length := small
			if dir == "large" || dir == "medium" {
				length = big
			}
			edit(dir, int(float64(count(dir))*share+0.5), length)
		}
	}
	dropShare := func(share float64) {
		for _, dir := range largeGroups {
			drop(dir, int(float64(count(dir))*share+0.5))
		}
	}
	switch step {
	case 1:
		c.Description = "edit 10% of files in every group (4 KiB in small files, 1 MiB in large and medium), add 2 GiB, delete 2%"
		editShare(0.10, 4<<10, 1<<20)
		addN("photos", 128, 8<<20)
		addN("docs", 512, 1<<20)
		addN("notes", 2000, 4<<10)
		dropShare(0.02)
	case 2:
		c.Description = "edit 30% of files in every group, add 4 GiB, delete 5%"
		editShare(0.30, 4<<10, 1<<20)
		addN("large", 2, 1<<30)
		addN("photos", 128, 8<<20)
		addN("code", 4000, 64<<10)
		dropShare(0.05)
	case 3:
		c.Description = "edit 20% of files in every group, add 1 GiB, delete 10%"
		editShare(0.20, 4<<10, 1<<20)
		addN("medium", 8, 128<<20)
		dropShare(0.10)
	case 4:
		c.Description = "rewrite 10% of photos and docs in full, edit 10% of the rest, add 1 GiB, delete 3%"
		edit("photos", count("photos")/10, 8<<20)
		edit("docs", count("docs")/10, 1<<20)
		for _, dir := range []string{"large", "medium", "code", "notes"} {
			length := int64(4 << 10)
			if dir == "large" || dir == "medium" {
				length = 1 << 20
			}
			edit(dir, count(dir)/10, length)
		}
		addN("docs", 1024, 1<<20)
		dropShare(0.03)
	case 5:
		c.Description = "many small files: edit 30% of notes and code, add 10,000 notes, delete 5% of notes"
		edit("notes", count("notes")*3/10, 4<<10)
		edit("code", count("code")*3/10, 4<<10)
		addN("notes", 10000, 4<<10)
		drop("notes", count("notes")/20)
	default:
		t.h.t.Fatal("unknown large hotspot step", step)
	}
}
