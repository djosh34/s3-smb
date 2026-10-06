//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// s3Outage cuts s3-smb off from MinIO for 5 minutes in the middle of a
// backup. The backup must only slow down: the Mac waits on the writes and
// FLUSHes that S3 holds up, and the same backup then completes without a
// failure or a reconnect refusal. Both backups must restore.
func (h *harness) s3Outage() result {
	// Keep forwarding alive for shutdown after a stage timeout. finish closes it.
	proxy, err := netfault.New(context.WithoutCancel(h.ctx), "127.0.0.1:19000")
	h.must(err)
	h.s3Proxy, h.endpoint = proxy, "http://"+proxy.Address()
	outcome := h.baseline()
	outcome.Scenario = "s3-outage"
	h.enableSMBLogging()
	h.randomFile("later.bin", 4<<30)
	h.must(h.proofDir.WriteFile("nested/message.txt", []byte("changed before the S3 outage\n"), 0o600))
	updated, _ := h.manifest(h.proof, h.evidenceDir, "updated-tree.json")
	before := h.objects("chunks/")
	started := time.Now().UTC()
	h.startBackup("outage")
	attempt := helpers.DropAttempt{StatusAtCut: h.bandWrites(before), CutAt: time.Now().UTC()}
	h.must(proxy.Drop())
	h.t.Log("s3-outage-start")
	// waitFor also fails when s3-smb exits.
	h.must(h.waitFor("S3 outage", 6*time.Minute, 5*time.Second, func() (bool, error) {
		if h.backup.exited() {
			return false, errors.New("the backup ended during the S3 outage")
		}
		return time.Since(attempt.CutAt) >= 5*time.Minute, nil
	}))
	status := h.run(2*time.Minute, "/usr/bin/tmutil", "status")
	running, err := helpers.Running(status)
	h.must(err)
	if !running {
		h.t.Fatal("Time Machine stopped during the S3 outage")
	}
	proxy.Restore()
	attempt.RestoredAt = time.Now().UTC()
	h.t.Log("s3-outage-end", status)
	commandErr := h.completeBackup("outage")
	attempt.Completed = commandErr == nil
	if commandErr != nil {
		attempt.CommandError = commandErr.Error()
	}
	attempt.Log = h.dropLog(started.Add(-30*time.Second), time.Now().UTC())
	h.save("s3-outage-result.json", attempt)
	h.must(commandErr)
	if attempt.Log.Failed || attempt.Log.Refused || attempt.Log.BackupStarts != 1 || attempt.Log.TimedOut != 0 {
		h.t.Fatalf("the backup did not go on through the S3 outage as one backup: %+v", attempt.Log)
	}
	h.must(h.detach())
	h.mount()
	latest := h.remoteBackup("after-outage", "")
	if filepath.Base(latest) == outcome.Baseline {
		h.t.Fatal("the backup during the S3 outage did not complete")
	}
	outcome.Resumed = filepath.Base(latest)
	outcome.ResumedRestore = h.restore(latest, updated, "restore-after-outage")
	h.must(h.detach())
	outcome.BaselineRestore = h.restoreBackup(outcome.Baseline, h.reference(), "restore-baseline")
	return outcome
}

// bandWrites waits until the running backup copies and has uploaded chunks
// that before did not have. It returns the last tmutil status.
func (h *harness) bandWrites(before map[string]int64) string {
	var previous, current string
	h.must(h.waitFor("band writes", 30*time.Minute, 2*time.Second, func() (bool, error) {
		if h.backup.exited() {
			return false, errors.New("Time Machine ended before band writes were seen")
		}
		previous, current = current, h.run(2*time.Minute, "/usr/bin/tmutil", "status")
		return helpers.BandWriteReady(previous, current) && helpers.CheckRemoteChange(before, h.objects("chunks/")) == nil, nil
	}))
	h.t.Log("band-writes", current)
	return current
}
