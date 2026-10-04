//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

func (h *harness) baseline() map[string]any {
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
	h.destinationSetup()
	h.createTree()
	h.checkExclusions()
	h.startBackup("baseline")
	completed, err := h.completeBackup("baseline")
	h.must(err)
	h.must(h.detach(h.ctx))
	h.mount()
	selected := h.remoteBackup("baseline", "")
	h.backupTree(selected)
	h.must(h.detach(h.ctx))
	h.metadata(completed, "baseline")
	return map[string]any{"baseline": filepath.Base(selected)}
}

func (h *harness) stopDaemon(abrupt bool) {
	p := h.daemon
	h.daemon = nil
	h.must(h.stop(p, abrupt, 45*time.Second))
}

func (h *harness) stopClient() {
	output, err := h.command(h.ctx, time.Minute, "", "/usr/bin/tmutil", "stopbackup")
	h.t.Log("stopbackup", output, err)
	if h.backup != nil {
		p := h.backup
		select {
		case <-p.done:
		case <-time.After(3 * time.Minute):
			h.must(p.cmd.Process.Kill())
			select {
			case <-p.done:
			case <-h.ctx.Done():
				h.t.Fatal(h.ctx.Err())
			}
		}
		h.must(p.log.Close())
		h.backup = nil
	}
	deadline := time.Now().Add(3 * time.Minute)
	for h.status() && time.Now().Before(deadline) {
		h.pause(5 * time.Second)
	}
	h.must(h.detach(h.ctx))
}

func (h *harness) copying() string {
	h.startBackup("interrupted")
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		if h.backup.exited() {
			h.t.Fatal("Time Machine ended before Copying was seen")
		}
		text := h.native("/usr/bin/tmutil", "status")
		if helpers.Copying(text) {
			h.event("time-machine-copying", map[string]any{"native_status": text})
			return text
		}
		h.pause(2 * time.Second)
	}
	h.t.Fatal("Time Machine did not reach Copying in 30 minutes")
	return ""
}

func (h *harness) cold() {
	if h.daemon != nil {
		h.stopDaemon(false)
	}
	h.must(os.RemoveAll(h.local))
	h.startDaemon("recover")
}

func (h *harness) scenario(name string) map[string]any {
	switch name {
	case "server-kill-restart", "server-kill-cold", "server-kill-cold-midpoint", "client-abort-cold", "machine-loss":
	default:
		h.t.Fatal("unknown interruption scenario", name)
	}
	midpoint := name == "server-kill-cold-midpoint"
	if midpoint {
		h.interval = "1m"
	}
	// The baseline routine waits for a native completed backup before any interruption.
	result := h.baseline()
	size := int64(1 << 30)
	if midpoint {
		size = 4 << 30
	}
	h.randomFile("later.bin", size)
	h.must(os.WriteFile(filepath.Join(h.proof, "nested/message.txt"), []byte("changed after the baseline\n"), 0o600))
	updated := filepath.Join(h.evidence, "updated-tree.jsonl")
	h.manifest(h.proof, updated)
	// Take P1 last so cold and machine-loss kills precede the next scheduled point.
	p1 := h.metadata(time.Now().UTC(), "pre-interruption")
	before := h.objects("s3-smb/chunks/")
	atKill := h.copying()
	point := p1
	if midpoint {
		point = h.metadata(time.Now().UTC(), "midpoint")
		atKill = h.native("/usr/bin/tmutil", "status")
		if !helpers.Copying(atKill) {
			h.t.Fatal("Time Machine stopped copying before the metadata backup landed")
		}
	}
	if name == "client-abort-cold" {
		aborted := time.Now().UTC()
		for _, args := range [][]string{{"/usr/bin/tmutil", "stopbackup"}, {"/usr/bin/pkill", "-9", "-x", "backupd"}} {
			output, err := h.command(h.ctx, time.Minute, "", args...)
			h.t.Log("client abort", output, err)
		}
		for _, path := range mountpoints(h.native("/sbin/mount")) {
			output, err := h.command(h.ctx, 2*time.Minute, "", "/sbin/umount", "-f", path)
			h.t.Log("forced client unmount", output, err)
		}
		h.stopClient()
		// Check before the metadata wait so housekeeping cannot supply the change.
		after := h.objects("s3-smb/chunks/")
		h.must(helpers.CheckRemoteChange(before, after))
		h.save("interrupted-chunks.json", map[string]any{"before": before, "after": after, "at_kill": atKill}, false)
		point = h.metadata(aborted, "after-abort")
		h.cold()
	} else {
		h.stopDaemon(true)
		h.stopClient()
		after := h.objects("s3-smb/chunks/")
		h.must(helpers.CheckRemoteChange(before, after))
		h.save("interrupted-chunks.json", map[string]any{"before": before, "after": after, "at_kill": atKill}, false)
		if name == "server-kill-restart" {
			h.startDaemon("restart")
		} else if name != "machine-loss" {
			h.cold()
		}
	}
	result["scenario"], result["metadata_backup"], result["at_kill"] = name, point.Key, atKill
	if name == "machine-loss" {
		return result
	}
	if name == "server-kill-cold" && h.daemon.point != p1.Key {
		h.t.Fatalf("recovered from %s, expected P1 %s", h.daemon.point, p1.Key)
	}
	if (midpoint || name == "client-abort-cold") && h.daemon.point < point.Key {
		h.t.Fatalf("recovered from %s, expected %s or later", h.daemon.point, point.Key)
	}
	baseline, ok := result["baseline"].(string)
	if !ok {
		h.t.Fatal("missing baseline identifier")
	}
	h.mount()
	result["baseline_restore"] = h.restore(h.remoteBackup("baseline-recovered", baseline), filepath.Join(h.transfer, "reference/tree.jsonl"), "restore-baseline")
	h.must(h.detach(h.ctx))
	var latest string
	for _, label := range []string{"resumed", "resumed-retry"} {
		h.startBackup(label)
		// Time Machine may return 0 without completing a backup. The native list is the gate.
		if _, err := h.completeBackup(label); err != nil {
			h.event("resumed-backup-failed", map[string]any{"label": label, "error": err.Error()})
		}
		h.must(h.detach(h.ctx))
		h.mount()
		latest = h.remoteBackup(label, "")
		if filepath.Base(latest) != baseline {
			break
		}
		h.must(h.detach(h.ctx))
	}
	if filepath.Base(latest) == baseline {
		h.t.Fatal("no backup completed after recovery")
	}
	result["resumed_restore"] = h.restore(latest, updated, "restore-resumed")
	result["resumed"], result["recovered_from"], result["chunk_objects_end"] = filepath.Base(latest), h.daemon.point, len(h.objects("s3-smb/chunks/"))
	h.must(h.detach(h.ctx))
	return result
}

func (h *harness) recoverStore() map[string]any {
	h.must(absent(h.local))
	h.must(absent(filepath.Join(h.work, "objects")))
	data, err := os.ReadFile(filepath.Join(h.transfer, "reference/recovery.json"))
	h.must(err)
	var result map[string]any
	h.must(json.Unmarshal(data, &result))
	baseline, ok := result["baseline"].(string)
	if !ok || baseline == "" {
		h.t.Fatal("missing baseline identifier")
	}
	h.platform(true)
	tar := filepath.Join(h.transfer, "store.tar")
	h.run(30*time.Minute, "/usr/bin/tar", "-C", h.work, "-xf", tar)
	h.must(os.Remove(tar))
	h.services(false)
	h.startDaemon("recover")
	if point, ok := result["metadata_backup"].(string); ok && point != h.daemon.point {
		h.t.Fatalf("recovered from %s, expected %s", h.daemon.point, point)
	}
	// No fixture is created on this Mac. Only hashes and the object store crossed runners.
	h.must(absent(h.proof))
	h.mount()
	result["baseline_restore"] = h.restore(h.remoteBackup("baseline", baseline), filepath.Join(h.transfer, "reference/tree.jsonl"), "restore-baseline")
	result["recovered_from"] = h.daemon.point
	h.must(h.detach(h.ctx))
	return result
}

func (h *harness) finish() {
	if h.finished {
		return
	}
	h.finished = true
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	var failures []error
	attempt := func(err error) {
		if err != nil {
			failures = append(failures, err)
			h.t.Error(err)
		}
	}
	if h.backup != nil {
		attempt(errors.New("owned Time Machine startbackup still active at final cleanup"))
		_, err := h.command(ctx, time.Minute, "", "/usr/bin/tmutil", "stopbackup")
		attempt(err)
		p := h.backup
		// The client can exit nonzero after a deliberately cancelled backup.
		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			attempt(p.cmd.Process.Kill())
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				attempt(errors.New("backup client did not exit"))
			}
		}
		attempt(p.log.Close())
		h.backup = nil
	}
	if h.daemon != nil {
		attempt(h.detach(ctx))
		attempt(h.stop(h.daemon, false, 45*time.Second))
		h.daemon = nil
	}
	if h.minio != nil {
		attempt(h.stop(h.minio, false, 30*time.Second))
		h.minio = nil
	}
	for _, args := range [][]string{{"/usr/bin/tmutil", "status"}, {"/sbin/mount"}, {"/usr/bin/hdiutil", "info", "-plist"}, {"/bin/df", "-k"}, {"/usr/bin/log", "show", "--style", "json", "--last", "6h", "--info", "--debug", "--predicate", `process == "backupd" OR process == "backupd-helper" OR process == "tmutil" OR process == "NetAuthSysAgent" OR subsystem BEGINSWITH "com.apple.smb" OR senderImagePath CONTAINS "smbfs"`}} {
		output, err := h.command(ctx, 90*time.Second, "", args...)
		if err != nil {
			h.t.Log("diagnostic failed", args, output, err)
		}
	}
	messages := make([]string, len(failures))
	for index, err := range failures {
		messages[index] = err.Error()
	}
	data, err := json.Marshal(map[string]any{"errors": messages})
	attempt(err)
	attempt(os.WriteFile(filepath.Join(h.evidence, "cleanup.json"), data, 0o600))
	// All other failures are already in the verbose go test log and command evidence.
	if h.t.Failed() {
		attempt(os.WriteFile(filepath.Join(h.evidence, "failure.txt"), []byte(strings.Join(messages, "\n")+"\nSee harness.log and commands.jsonl for the test failure.\n"), 0o600))
	}
}
