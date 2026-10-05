//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// networkScenario runs network-drop or network-outage. Every SMB client of the
// Mac, including Time Machine and restore mounts, connects through a fault
// proxy that cuts the connection during a backup.
func (h *harness) networkScenario(name string) result {
	// Keep forwarding alive for detach after a stage timeout. finish closes it.
	proxy, err := netfault.New(context.WithoutCancel(h.ctx), "127.0.0.1:1445")
	h.must(err)
	h.proxy, h.smbAddress = proxy, proxy.Address()
	h.save("network-proxy.json", map[string]string{"client": h.smbAddress, "server": "127.0.0.1:1445"})
	outcome := h.baseline()
	h.enableSMBLogging()
	outcome.Scenario = name
	if name == "network-outage" {
		return h.networkOutage(outcome)
	}
	var updated []helpers.Entry
	report, err := helpers.RunDropAttempts(func(number int) helpers.DropAttempt {
		attempt, tree, commandErr := h.dropBackup(number, false)
		updated = tree
		if commandErr != nil {
			h.t.Log("interrupted-backup-exit", commandErr)
		}
		h.stopClient()
		if attempt.Log.Refused {
			h.t.Log("not tested: macOS refused reconnect", number)
		}
		return attempt
	})
	outcome.NetworkDrop = &report
	h.save("network-drop-result.json", report)
	h.must(err)
	if report.Status == "not tested" {
		return outcome
	}
	h.mount()
	latest := h.remoteBackup("same-backup", "")
	if filepath.Base(latest) == outcome.Baseline {
		h.t.Fatal("cut backup produced no completed backup")
	}
	outcome.Resumed = filepath.Base(latest)
	outcome.ResumedRestore = h.restore(latest, updated, "restore-same-backup")
	h.must(h.detach())
	h.mount()
	baseline := h.remoteBackup("baseline-intact", outcome.Baseline)
	outcome.BaselineRestore = h.restore(baseline, h.reference(), "restore-baseline")
	h.must(h.detach())
	return outcome
}

func (h *harness) networkOutage(outcome result) result {
	attempt, updated, commandErr := h.dropBackup(1, true)
	h.save("network-outage-result.json", attempt)
	h.stopClient()
	h.mount()
	latest := h.remoteBackup("after-outage", "")
	h.must(helpers.CheckOutage(attempt.CutAt, attempt.RestoredAt, commandErr, filepath.Base(latest), outcome.Baseline))
	outcome.BaselineRestore = h.restore(latest, h.reference(), "restore-baseline")
	h.must(h.detach())
	latest = h.resumeBackup(outcome.Baseline, true)
	outcome.Resumed, outcome.ResumedRestore = filepath.Base(latest), h.restore(latest, updated, "restore-next-backup")
	h.must(h.detach())
	return outcome
}

// dropBackup changes the test tree, starts a backup and cuts the connection
// while Time Machine writes bands. A short drop restores forwarding after five
// seconds. A long one waits at least 45 seconds and until the backup fails.
func (h *harness) dropBackup(number int, long bool) (helpers.DropAttempt, []helpers.Entry, error) {
	label := fmt.Sprintf("network-attempt-%d", number)
	if number > 1 {
		h.must(h.proofDir.Remove("later.bin"))
	}
	h.randomFile("later.bin", 4<<30)
	h.must(h.proofDir.WriteFile("nested/message.txt", []byte(label+"\n"), 0o600))
	updated, _ := h.manifest(h.proof, h.evidenceDir, label+"-tree.json")
	before := h.objects("s3-smb/chunks/")
	started := time.Now().UTC()
	h.startBackup(label)
	var previous, current string
	h.must(h.waitFor("large band writes", 30*time.Minute, 2*time.Second, func() (bool, error) {
		if h.backup.exited() {
			return false, errors.New("backup ended before the connection cut")
		}
		current = h.run(2*time.Minute, "/usr/bin/tmutil", "status")
		ready := helpers.BandWriteReady(previous, current)
		previous = current
		if !ready {
			return false, nil
		}
		// Prove this attempt uploaded chunks, then resample immediately before cutting.
		after := h.objects("s3-smb/chunks/")
		if err := helpers.CheckRemoteChange(before, after); err != nil {
			h.t.Log("waiting for remote band data", err)
			return false, nil
		}
		current = h.run(2*time.Minute, "/usr/bin/tmutil", "status")
		ready = helpers.BandWriteReady(previous, current)
		previous = current
		return ready, nil
	}))
	attempt := helpers.DropAttempt{StatusAtCut: current, CutAt: time.Now().UTC()}
	h.must(h.proxy.Drop())
	h.save(label+"-cut.json", attempt)
	if long {
		h.pause(45 * time.Second)
		// Do not stopbackup or kill the client. Its own failure must end this run.
		h.must(h.waitFor("visible failure during outage", 10*time.Minute, time.Second, func() (bool, error) {
			return h.backup.exited(), nil
		}))
		if h.backup.err == nil {
			h.t.Fatal("long outage ended without a visible tmutil failure")
		}
	} else {
		h.pause(5 * time.Second)
	}
	h.proxy.Restore()
	attempt.RestoredAt = time.Now().UTC()
	_, commandErr := h.completeBackup(label)
	attempt.Completed = commandErr == nil
	if commandErr != nil {
		attempt.CommandError = commandErr.Error()
	}
	attempt.Log = h.dropLog(started, time.Now().UTC())
	h.save(label+"-result.json", attempt)
	return attempt, updated, commandErr
}

// enableSMBLogging sets the kernel SMB log level to 1, which logs the
// reconnect refusal. smbfs must be loaded, so call it after the first mount.
// finish restores the previous level.
func (h *harness) enableSMBLogging() {
	previous := h.sysctlSMBLogLevel()
	h.smbLogLevel = previous
	h.run(time.Minute, "/usr/sbin/sysctl", "-w", "net.smb.fs.loglevel=1")
	active := h.sysctlSMBLogLevel()
	h.save("smb-kernel-logging.json", map[string]string{"previous": previous, "active": active})
	if active == "0" {
		h.t.Fatal("SMB warning logging is still disabled")
	}
}

func (h *harness) sysctlSMBLogLevel() string {
	level := strings.TrimSpace(h.run(time.Minute, "/usr/sbin/sysctl", "-n", "net.smb.fs.loglevel"))
	_, err := strconv.ParseUint(level, 10, 32)
	h.must(err)
	return level
}

func (h *harness) restoreSMBLogging() error {
	if h.smbLogLevel == "" {
		return nil
	}
	_, err := h.try(time.Minute, "/usr/sbin/sysctl", "-w", "net.smb.fs.loglevel="+h.smbLogLevel)
	return err
}

func (h *harness) dropLog(start, end time.Time) helpers.DropLog {
	format := "2006-01-02 15:04:05-0700"
	h.run(2*time.Minute, "/usr/bin/log", "show", "--style", "json", "--start", start.Format(format), "--end", end.Add(time.Second).Format(format), "--info", "--debug", "--predicate", `process == "backupd" OR senderImagePath CONTAINS "smbfs"`)
	// try leaves log show output on disk rather than returning it.
	data, err := h.evidenceDir.ReadFile(fmt.Sprintf("%04d-log.log", h.serial))
	h.must(err)
	log, err := helpers.ParseDropLog(data)
	h.must(err)
	return log
}
