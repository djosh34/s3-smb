//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"fmt"
	"path/filepath"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// incremental changes the test tree, backs it up and returns the new backup
// with the tree's manifest. previous is the backup before it.
func (h *harness) incremental(label, previous string, change func()) (string, []helpers.Entry) {
	change()
	tree, _ := h.manifest(h.proof, h.evidenceDir, label+"-tree.json")
	h.startBackup(label)
	h.must(h.completeBackup(label))
	h.must(h.detach())
	h.mount()
	latest := filepath.Base(h.remoteBackup(label, ""))
	h.must(h.detach())
	if latest == previous {
		h.t.Fatal("incremental backup did not produce a new backup", label)
	}
	return latest, tree
}

// restoreBackup restores backup and requires it to match tree.
func (h *harness) restoreBackup(backup string, tree []helpers.Entry, name string) helpers.Counts {
	h.mount()
	counts := h.restore(h.remoteBackup(name, backup), tree, name)
	h.must(h.detach())
	return counts
}

// incrementals makes a first backup and three incrementals that add, change
// and delete files, then restores the latest and the first.
func (h *harness) incrementals() result {
	outcome := h.baseline()
	outcome.Scenario = "incrementals"
	latest := outcome.Baseline
	var tree []helpers.Entry
	for step := 1; step <= 3; step++ {
		label := fmt.Sprintf("incremental-%d", step)
		latest, tree = h.incremental(label, latest, func() {
			h.randomFile(fmt.Sprintf("added-%d.bin", step), 256<<20)
			h.must(h.proofDir.WriteFile("nested/message.txt", []byte(label+"\n"), 0o600))
			if step == 3 {
				h.must(h.proofDir.Remove("added-1.bin"))
			}
		})
	}
	outcome.Resumed = latest
	outcome.ResumedRestore = h.restoreBackup(latest, tree, "restore-latest")
	outcome.BaselineRestore = h.restoreBackup(outcome.Baseline, h.reference(), "restore-baseline")
	return outcome
}
