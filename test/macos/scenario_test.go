//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type result struct {
	NetworkDrop     *helpers.DropReport `json:"network_drop,omitempty"`
	Baseline        string              `json:"baseline"`
	Scenario        string              `json:"scenario,omitempty"`
	AtKill          string              `json:"at_kill,omitempty"`
	Resumed         string              `json:"resumed,omitempty"`
	RestoredFrom    string              `json:"restored_from,omitempty"`
	BaselineRestore helpers.Counts      `json:"baseline_restore,omitempty"`
	ResumedRestore  helpers.Counts      `json:"resumed_restore,omitempty"`
	ChunkObjectsEnd int                 `json:"chunk_objects_end,omitempty"`
}

// baseline starts fresh storage and s3-smb, then makes and checks the first
// Time Machine backup, and waits for a database copy that holds it.
func (h *harness) baseline() result {
	h.must(absent(filepath.Join(h.work, "objects")))
	h.must(absent(h.local))
	h.platform(false)
	h.services(true)
	h.startDaemon("initialize")
	h.mount()
	entries, err := os.ReadDir(h.share)
	h.must(err)
	if len(entries) != 0 {
		h.t.Fatal("initial application share not empty")
	}
	// The server lets one client in at a time. Unmount before tmutil connects.
	h.must(h.detachShares())
	h.destinationSetup()
	h.createTree()
	h.checkExclusions()
	h.startBackup("baseline")
	h.must(h.completeBackup("baseline"))
	h.must(h.detach())
	h.mount()
	selected := h.remoteBackup("baseline", "")
	h.backupTree(selected)
	h.must(h.detach())
	h.newCopy("baseline")
	return result{Baseline: filepath.Base(selected)}
}

func (h *harness) stopDaemon(abrupt bool) {
	p := h.daemon
	h.daemon = nil
	h.must(stop(p, abrupt))
}

func (h *harness) stopBackup(limit time.Duration) error {
	output, err := h.try(time.Minute, "/usr/bin/tmutil", "stopbackup")
	h.t.Log("stopbackup", output, err)
	if h.backup == nil {
		return nil
	}
	p := h.backup
	select {
	case <-p.done:
	case <-h.ctx.Done():
		return h.ctx.Err()
	case <-time.After(limit):
		if err := p.cmd.Process.Kill(); err != nil {
			return err
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return errors.New("Time Machine client was not reaped")
		}
	}
	p.cancel()
	h.backup = nil
	return p.log.Close()
}

func (h *harness) stopClient() {
	h.must(h.stopBackup(3 * time.Minute))
	err := h.waitFor("backupd stop", 3*time.Minute, 5*time.Second, func() (bool, error) { return !h.status(), nil })
	if err != nil {
		if h.ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			h.must(err)
		}
		h.t.Log("backupd still stopping before forced detach", err)
	}
	h.must(h.detach())
}

func (h *harness) copying() string {
	h.startBackup("interrupted")
	var text string
	h.must(h.waitFor("Time Machine Copying", 30*time.Minute, 2*time.Second, func() (bool, error) {
		if h.backup.exited() {
			return false, errors.New("Time Machine ended before Copying was seen")
		}
		text = h.run(2*time.Minute, "/usr/bin/tmutil", "status")
		return helpers.Copying(text), nil
	}))
	h.t.Log("time-machine-copying", text)
	return text
}

// cold stops s3-smb, if it runs, and starts it on a new data folder. The new
// server must restore the newest database copy, which it returns.
func (h *harness) cold() string {
	if h.daemon != nil {
		h.stopDaemon(false)
	}
	newest := helpers.NewestCopy(h.objects("db/"))
	h.must(h.workDir.RemoveAll("daemon"))
	h.startDaemon("recover")
	if h.daemon.point != newest {
		h.t.Fatalf("restored %s, expected the newest copy %s", h.daemon.point, newest)
	}
	return newest
}

func (h *harness) scenario(name string) result {
	switch name {
	case "network-drop", "network-outage":
		return h.networkScenario(name)
	case "server-kill-restart", "launchd-kill-restart", "server-kill-cold", "server-kill-cold-midpoint", "client-abort-cold", "machine-loss":
	default:
		h.t.Fatal("unknown interruption scenario", name)
	}
	outcome := h.baseline()
	launchdPID := 0
	if name == "launchd-kill-restart" {
		launchdPID = h.startLaunchd()
	}
	h.randomFile("later.bin", 1<<30)
	h.must(h.proofDir.WriteFile("nested/message.txt", []byte("changed after the baseline\n"), 0o600))
	updated, _ := h.manifest(h.proof, h.evidenceDir, "updated-tree.json")
	before := h.objects("chunks/")
	atKill := h.copying()
	var after map[string]int64
	newPID := 0
	switch name {
	case "client-abort-cold":
		for _, args := range [][]string{{"/usr/bin/tmutil", "stopbackup"}, {"/usr/bin/pkill", "-9", "-x", "backupd"}} {
			output, err := h.try(time.Minute, args...)
			h.t.Log("client abort", output, err)
		}
		for _, path := range h.mountpoints(h.run(2*time.Minute, "/sbin/mount")) {
			output, err := h.try(2*time.Minute, "/sbin/umount", "-f", path)
			h.t.Log("forced client unmount", output, err)
		}
	case "launchd-kill-restart":
		after, newPID = h.killLaunchd(launchdPID, atKill)
	default:
		h.stopDaemon(true)
	}
	h.stopClient()
	// Check before any restart. launchd captured this before its restart.
	if after == nil {
		after = h.objects("chunks/")
	}
	h.must(helpers.CheckRemoteChange(before, after))
	h.save("interrupted-chunks.json", map[string]any{"before": before, "after": after, "at_kill": atKill})
	outcome.Scenario, outcome.AtKill = name, atKill
	switch name {
	case "machine-loss":
		return outcome
	case "server-kill-restart":
		h.startDaemon("restart")
	case "server-kill-cold":
		outcome.RestoredFrom = h.cold()
	case "server-kill-cold-midpoint":
		// The restart's start copy holds what the interrupted backup flushed,
		// so the cold start restores a copy from the middle of a backup.
		h.startDaemon("restart")
		outcome.RestoredFrom = h.cold()
	case "client-abort-cold":
		h.newCopy("after-abort")
		outcome.RestoredFrom = h.cold()
	}
	h.mount()
	outcome.BaselineRestore = h.restore(h.remoteBackup("baseline-recovered", outcome.Baseline), h.reference(), "restore-baseline")
	h.must(h.detach())
	latest := h.resumeBackup(outcome.Baseline, name == "launchd-kill-restart")
	outcome.ResumedRestore = h.restore(latest, updated, "restore-resumed")
	if name == "launchd-kill-restart" {
		h.checkLaunchdPID(newPID)
	}
	outcome.Resumed, outcome.ChunkObjectsEnd = filepath.Base(latest), len(h.objects("chunks/"))
	h.must(h.detach())
	return outcome
}

func (h *harness) resumeBackup(baseline string, requireNext bool) string {
	for _, label := range []string{"resumed", "resumed-retry"} {
		h.startBackup(label)
		if err := h.completeBackup(label); err != nil {
			if requireNext {
				h.must(err)
			}
			h.t.Log("resumed-backup-failed", label, err)
		}
		h.must(h.detach())
		h.mount()
		latest := h.remoteBackup(label, "")
		if filepath.Base(latest) != baseline {
			return latest
		}
		h.must(h.detach())
		if requireNext {
			h.t.Fatal("next backup did not produce a completed Time Machine backup")
		}
	}
	h.t.Fatal("no backup completed after recovery")
	return ""
}

// recoverStore starts s3-smb with a new data folder on a fresh Mac, on the
// store that a backup or machine-loss job exported. It must restore the
// newest database copy. Then the baseline backup is restored.
func (h *harness) recoverStore() result {
	h.must(absent(h.local))
	h.must(absent(filepath.Join(h.work, "objects")))
	var outcome result
	h.must(readJSON(h.transferDir, "reference/recovery.json", &outcome))
	if outcome.Baseline == "" {
		h.t.Fatal("missing baseline identifier")
	}
	h.platform(true)
	h.run(30*time.Minute, "/usr/bin/tar", "-C", h.work, "-xf", filepath.Join(h.transfer, "store.tar"))
	h.must(h.transferDir.Remove("store.tar"))
	h.services(false)
	newest := helpers.NewestCopy(h.objects("db/"))
	h.startDaemon("recover")
	if h.daemon.point != newest {
		h.t.Fatalf("restored %s, expected the newest copy %s", h.daemon.point, newest)
	}
	// No source tree is created here. Only hashes and S3 objects crossed runners.
	h.must(absent(h.proof))
	h.mount()
	outcome.BaselineRestore = h.restore(h.remoteBackup("baseline", outcome.Baseline), h.reference(), "restore-baseline")
	outcome.RestoredFrom = h.daemon.point
	h.must(h.detach())
	return outcome
}

// finish releases everything the run started, also after a failure or timeout,
// and saves final diagnostics.
func (h *harness) finish() {
	if h.finished {
		return
	}
	h.finished = true
	parent := h.ctx
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer func() { cancel(); h.ctx = parent }()
	report := func(err error) {
		if err != nil {
			h.t.Error(err)
		}
	}
	if h.proxy != nil {
		h.proxy.Restore()
	}
	// Clients get five minutes, which leaves two for the launchd job and services.
	clientCtx, cancelClients := context.WithTimeout(ctx, 5*time.Minute)
	h.ctx = clientCtx
	if h.backup != nil {
		h.t.Error("owned Time Machine startbackup still active at final cleanup")
		report(h.stopBackup(30 * time.Second))
	}
	if h.daemon != nil || h.launchdPlist != "" || len(h.attachments) != 0 {
		report(h.detach())
	}
	cancelClients()
	h.ctx = ctx
	report(h.unloadLaunchd())
	report(h.restoreSMBLogging())
	if h.proxy != nil {
		report(h.proxy.Close())
		h.proxy = nil
	}
	if h.daemon != nil {
		report(stop(h.daemon, false))
		h.daemon = nil
	}
	if h.minio != nil {
		report(stop(h.minio, false))
		h.minio = nil
	}
	for _, args := range [][]string{{"/usr/bin/tmutil", "status"}, {"/sbin/mount"}, {"/usr/bin/hdiutil", "info", "-plist"}, {"/bin/df", "-k"}, {"/usr/bin/log", "show", "--style", "json", "--last", "6h", "--info", "--debug", "--predicate", `process == "backupd" OR process == "backupd-helper" OR process == "tmutil" OR process == "NetAuthSysAgent" OR subsystem BEGINSWITH "com.apple.smb" OR senderImagePath CONTAINS "smbfs"`}} {
		if _, err := h.try(90*time.Second, args...); err != nil {
			h.t.Log("diagnostic failed", args, err)
		}
	}
	h.t.Log("cleanup finished")
}
