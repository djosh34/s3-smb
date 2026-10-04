//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type result struct {
	NetworkDrop     *helpers.DropReport `json:"network_drop,omitempty"`
	Baseline        string              `json:"baseline"`
	Scenario        string              `json:"scenario,omitempty"`
	MetadataBackup  string              `json:"metadata_backup,omitempty"`
	AtKill          string              `json:"at_kill,omitempty"`
	Resumed         string              `json:"resumed,omitempty"`
	RecoveredFrom   string              `json:"recovered_from,omitempty"`
	BaselineRestore helpers.Counts      `json:"baseline_restore,omitempty"`
	ResumedRestore  helpers.Counts      `json:"resumed_restore,omitempty"`
	ChunkObjectsEnd int                 `json:"chunk_objects_end,omitempty"`
}

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
	if os.Getenv("MAC_PHASE") == "m4" {
		h.must(helpers.MacFeatures(func(args ...string) (string, error) {
			return h.try(5*time.Minute, args...)
		}, h.share, filepath.Join(h.bin, "fullsync")))
		h.t.Log("m4-share-features-passed")
	}
	h.destinationSetup()
	h.createTree()
	h.checkExclusions()
	h.startBackup("baseline")
	completed, err := h.completeBackup("baseline")
	h.must(err)
	h.must(h.detach())
	h.mount()
	selected := h.remoteBackup("baseline", "")
	h.backupTree(selected)
	h.must(h.detach())
	h.metadata(completed, "baseline")
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

func (h *harness) cold() {
	if h.daemon != nil {
		h.stopDaemon(false)
	}
	h.must(os.RemoveAll(h.local))
	h.startDaemon("recover")
}

func (h *harness) scenario(name string) result {
	switch name {
	case "network-drop", "network-outage":
		return h.networkScenario(name)
	case "server-kill-restart", "launchd-kill-restart", "server-kill-cold", "server-kill-cold-midpoint", "client-abort-cold", "machine-loss":
	default:
		h.t.Fatal("unknown interruption scenario", name)
	}
	midpoint := name == "server-kill-cold-midpoint"
	if midpoint {
		h.interval = "1m"
	}
	outcome := h.baseline()
	launchdPID := 0
	if name == "launchd-kill-restart" {
		launchdPID = h.startLaunchd()
	}
	size := int64(1 << 30)
	if midpoint {
		size = 4 << 30
	}
	h.randomFile("later.bin", size)
	h.must(os.WriteFile(filepath.Join(h.proof, "nested/message.txt"), []byte("changed after the baseline\n"), 0o600))
	updated := filepath.Join(h.evidence, "updated-tree.json")
	h.manifest(h.proof, updated)
	// Take P1 last, so cold and machine-loss kills precede the next scheduled point.
	p1 := h.metadata(time.Now().UTC(), "pre-interruption")
	before := h.objects("s3-smb/chunks/")
	atKill := h.copying()
	point := p1
	if midpoint {
		point = h.metadata(time.Now().UTC(), "midpoint")
		atKill = h.run(2*time.Minute, "/usr/bin/tmutil", "status")
		if !helpers.Copying(atKill) {
			h.t.Fatal("Time Machine stopped copying before the metadata backup landed")
		}
	}
	aborted := time.Now().UTC()
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
	// Check before any recovery or metadata wait. launchd captured this before restart.
	if after == nil {
		after = h.objects("s3-smb/chunks/")
	}
	h.must(helpers.CheckRemoteChange(before, after))
	h.save("interrupted-chunks.json", map[string]any{"before": before, "after": after, "at_kill": atKill})
	outcome.Scenario, outcome.AtKill = name, atKill
	if name == "machine-loss" {
		outcome.MetadataBackup = p1.Key
		return outcome
	}
	if name == "server-kill-restart" {
		h.startDaemon("restart")
	} else if name != "launchd-kill-restart" {
		if name == "client-abort-cold" {
			point = h.metadata(aborted, "after-abort")
		}
		h.cold()
		if name == "server-kill-cold" && h.daemon.point != p1.Key {
			h.t.Fatalf("recovered from %s, expected P1 %s", h.daemon.point, p1.Key)
		}
		if name != "server-kill-cold" && h.daemon.point < point.Key {
			h.t.Fatalf("recovered from %s, expected %s or later", h.daemon.point, point.Key)
		}
	}
	outcome.MetadataBackup = point.Key
	h.mount()
	outcome.BaselineRestore = h.restore(h.remoteBackup("baseline-recovered", outcome.Baseline), filepath.Join(h.transfer, "reference/tree.json"), "restore-baseline")
	h.must(h.detach())
	latest := h.resumeBackup(outcome.Baseline, name == "launchd-kill-restart")
	outcome.ResumedRestore = h.restore(latest, updated, "restore-resumed")
	if name == "launchd-kill-restart" {
		h.checkLaunchdPID(newPID)
	}
	outcome.Resumed, outcome.ChunkObjectsEnd = filepath.Base(latest), len(h.objects("s3-smb/chunks/"))
	if h.daemon != nil {
		outcome.RecoveredFrom = h.daemon.point
	}
	h.must(h.detach())
	return outcome
}

func (h *harness) resumeBackup(baseline string, requireNext bool) string {
	for _, label := range []string{"resumed", "resumed-retry"} {
		h.startBackup(label)
		if _, err := h.completeBackup(label); err != nil {
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

func (h *harness) recoverStore() result {
	h.must(absent(h.local))
	h.must(absent(filepath.Join(h.work, "objects")))
	data, err := os.ReadFile(filepath.Join(h.transfer, "reference/recovery.json"))
	h.must(err)
	var outcome result
	h.must(json.Unmarshal(data, &outcome))
	if outcome.Baseline == "" {
		h.t.Fatal("missing baseline identifier")
	}
	h.platform(true)
	tar := filepath.Join(h.transfer, "store.tar")
	h.run(30*time.Minute, "/usr/bin/tar", "-C", h.work, "-xf", tar)
	h.must(os.Remove(tar))
	h.services(false)
	h.startDaemon("recover")
	if outcome.MetadataBackup != "" && outcome.MetadataBackup != h.daemon.point {
		h.t.Fatalf("recovered from %s, expected %s", h.daemon.point, outcome.MetadataBackup)
	}
	// No source tree is created here. Only hashes and S3 objects crossed runners.
	h.must(absent(h.proof))
	h.mount()
	outcome.BaselineRestore = h.restore(h.remoteBackup("baseline", outcome.Baseline), filepath.Join(h.transfer, "reference/tree.json"), "restore-baseline")
	outcome.RecoveredFrom = h.daemon.point
	h.must(h.detach())
	return outcome
}

func (h *harness) finish() {
	if h.finished {
		return
	}
	h.finished = true
	parent := h.ctx
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	h.ctx = ctx
	defer func() { cancel(); h.ctx = parent }()
	report := func(err error) {
		if err != nil {
			h.t.Error(err)
		}
	}
	if h.proxy != nil {
		report(h.proxy.SetFault(netfault.Fault{}))
	}
	// Leave two minutes of the outer budget for bootout and stopping services.
	//nolint:contextcheck // Native commands use h.ctx, set to each callback's context before calls.
	report(helpers.Cleanup(ctx, 5*time.Minute, func(clientCtx context.Context) error {
		h.ctx = clientCtx
		var err error
		if h.backup != nil {
			h.t.Error("owned Time Machine startbackup still active at final cleanup")
			err = h.stopBackup(30 * time.Second)
		}
		if h.daemon != nil || h.launchdPlist != "" || len(h.attachments) != 0 {
			err = errors.Join(err, h.detach())
		}
		return err
	}, func(cleanupCtx context.Context) error {
		h.ctx = cleanupCtx
		return errors.Join(h.unloadLaunchd(), h.smbLogging.Restore(h.smbLogCommand))
	}))
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
