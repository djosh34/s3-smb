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

// s3Outage cuts s3-smb off from MinIO in the middle of a backup. A cut of 5
// minutes is one the server rides out: the Mac waits on the writes and
// FLUSHes that S3 holds up, and the same backup then completes without a
// failure, a reconnect refusal or a request macOS timed out. Both backups
// must restore.
//
// With long, the cut lasts 6.5 minutes: longer than the server waits on S3,
// shorter than its 8-minute bucket lock. Requests then fail with a device
// error. The backup may fail, if Time Machine shows it, or complete; either
// way every backup it reports must restore exactly.
func (h *harness) s3Outage(long bool) result {
	scenario, outage := "s3-outage", 5*time.Minute
	if long {
		scenario, outage = "s3-outage-long", 6*time.Minute+30*time.Second
	}
	// Keep forwarding alive for shutdown after a stage timeout. finish closes it.
	proxy, err := netfault.New(context.WithoutCancel(h.ctx), "127.0.0.1:19000")
	h.must(err)
	h.s3Proxy, h.endpoint = proxy, "http://"+proxy.Address()
	outcome := h.baseline()
	outcome.Scenario = scenario
	h.enableSMBLogging()
	h.randomFile("later.bin", 4<<30)
	h.must(h.proofDir.WriteFile("nested/message.txt", []byte("changed before the S3 outage\n"), 0o600))
	updated, _ := h.manifest(h.proof, h.evidenceDir, "updated-tree.json")
	before := h.objects("chunks/")
	started := time.Now().UTC()
	h.startBackup("outage")
	attempt := helpers.DropAttempt{StatusAtCut: h.bandWrites(before), CutAt: time.Now().UTC()}
	h.must(proxy.Drop())
	h.t.Log("s3-outage-start", outage)
	// waitFor also fails when s3-smb exits.
	h.must(h.waitFor("S3 outage", outage+time.Minute, 5*time.Second, func() (bool, error) {
		if h.backup.exited() && !long {
			return false, errors.New("the backup ended during the S3 outage")
		}
		return time.Since(attempt.CutAt) >= outage, nil
	}))
	status := h.run(2*time.Minute, "/usr/bin/tmutil", "status")
	running, err := helpers.Running(status)
	h.must(err)
	if !running && !long {
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
	h.save(scenario+"-result.json", attempt)
	h.t.Logf("%s: completed=%v failed=%v timed-out=%d failed-writes=%d", scenario, attempt.Completed, attempt.Log.Failed, attempt.Log.TimedOut, attempt.Log.FailedWrites)
	failed := commandErr != nil || attempt.Log.Failed
	switch {
	case !long:
		h.must(commandErr)
		if attempt.Log.Failed || attempt.Log.Refused || attempt.Log.BackupStarts != 1 || attempt.Log.TimedOut != 0 {
			h.t.Fatalf("the backup did not go on through the S3 outage as one backup: %+v", attempt.Log)
		}
	case failed:
		// Time Machine shows the failure. The earlier backup must still restore.
		h.t.Log("s3-outage-long-visible-failure", attempt.CommandError)
	}
	h.must(h.detach())
	// Any backup that is there after the outage must restore exactly, also
	// one Time Machine reported as failed.
	h.mount()
	latest := h.remoteBackup("after-outage", "")
	if filepath.Base(latest) != outcome.Baseline {
		outcome.Resumed = filepath.Base(latest)
		outcome.ResumedRestore = h.restore(latest, updated, "restore-after-outage")
	} else if !failed {
		h.t.Fatal("Time Machine reported the backup during the S3 outage as good, but it is not there")
	}
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
