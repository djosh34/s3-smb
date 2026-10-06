//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// rollback makes backup A, then kills s3-smb right after a database copy
// lands in the middle of backup B and wipes its data folder. A new server
// waits out the stale lock and restores that copy, so the bucket rolls back
// to the middle of B, as after losing the local disk. The Mac mounts fresh. A
// must restore, and backup C must hold the file that changed only in B.
// s3-smb makes a copy every 2 minutes here.
func (h *harness) rollback() result {
	outcome := h.baseline()
	outcome.Scenario = "rollback"
	h.randomFile("later.bin", 4<<30)
	h.must(h.proofDir.WriteFile("nested/message.txt", []byte("changed only in backup B\n"), 0o600))
	before := h.objects("chunks/")
	h.startBackup("b")
	h.bandWrites(before)
	landed := len(h.landed())
	h.must(h.waitFor("a copy landing in backup B", 10*time.Minute, 500*time.Millisecond, func() (bool, error) {
		if h.backup.exited() {
			return false, errors.New("backup B ended before a copy landed in it")
		}
		copies := h.landed()
		if len(copies) > landed {
			outcome.AtKill = copies[len(copies)-1]
			return true, nil
		}
		return false, nil
	}))
	h.stopDaemon(true)
	h.t.Log("killed-after-copy", outcome.AtKill)
	h.stopClient()
	outcome.RestoredFrom = h.cold()
	if outcome.RestoredFrom != outcome.AtKill {
		h.t.Fatalf("restored %s, expected the copy from the middle of backup B %s", outcome.RestoredFrom, outcome.AtKill)
	}
	outcome.BaselineRestore = h.restoreBackup(outcome.Baseline, h.reference(), "restore-a")
	h.must(h.proofDir.WriteFile("added-in-c.txt", []byte("added in backup C\n"), 0o600))
	updated, _ := h.manifest(h.proof, h.evidenceDir, "c-tree.json")
	latest := h.resumeBackup(outcome.Baseline, false)
	outcome.Resumed = filepath.Base(latest)
	outcome.ResumedRestore = h.restore(latest, updated, "restore-c")
	h.must(h.detach())
	return outcome
}

// landed returns the database copies that the running s3-smb landed.
func (h *harness) landed() []string {
	data, err := h.evidenceDir.ReadFile(h.daemon.name + ".log")
	h.must(err)
	return helpers.LandedCopies(string(data))
}
