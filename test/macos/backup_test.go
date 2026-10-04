//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"howett.net/plist"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type entity struct {
	Device string `plist:"dev-entry"`
	Mount  string `plist:"mount-point"`
}
type image struct {
	Path     string   `plist:"image-path"`
	Entities []entity `plist:"system-entities"`
}
type imageInfo struct {
	Images []image `plist:"images"`
}

func (h *harness) destinationSetup() {
	output := h.native("/usr/bin/tmutil", "setdestination", "smb://timemachine:synthetic-tm-control@127.0.0.1:1445/TimeMachine")
	if strings.Contains(output, "The backup destination could not be set.") {
		h.t.Fatal("tmutil could not set the destination despite exit 0")
	}
	text := h.native("/usr/bin/tmutil", "destinationinfo", "-X")
	var info struct {
		Destinations []struct {
			ID string `plist:"ID"`
		} `plist:"Destinations"`
	}
	_, err := plist.Unmarshal([]byte(text), &info)
	h.must(err)
	h.save("destination.json", info, false)
	if len(info.Destinations) != 1 || info.Destinations[0].ID == "" {
		h.t.Fatal("expected exactly one Time Machine destination with an ID")
	}
	h.destination = info.Destinations[0].ID
	// Port 1445 bypasses Apple's loopback 139/445 restriction. backupd needs the System keychain.
	keychain := "/Library/Keychains/System.keychain"
	output, err = h.command(h.ctx, 2*time.Minute, "", "/usr/bin/security", "find-internet-password", "-s", "127.0.0.1", "-a", "timemachine", keychain)
	h.t.Log("existing synthetic credential", output, err)
	attributes := []string{"-s", "127.0.0.1", "-a", "timemachine", "-P", "1445", "-r", "smb ", "-p", "TimeMachine"}
	args := append([]string{"/usr/bin/security", "add-internet-password", "-U"}, attributes...)
	args = append(args, "-T", "/System/Library/CoreServices/NetAuthAgent.app/Contents/MacOS/NetAuthSysAgent", "-T", "/System/Library/CoreServices/TimeMachine/backupd", "-w", "synthetic-tm-control", keychain)
	h.native(args...)
	h.native(append(append([]string{"/usr/bin/security", "find-internet-password"}, attributes...), keychain)...)
}

func (h *harness) status() bool {
	running, err := helpers.Running(h.native("/usr/bin/tmutil", "status"))
	h.must(err)
	return running
}

func (h *harness) startBackup(label string) {
	if h.backup != nil {
		h.t.Fatal("backup already active")
	}
	h.backup = h.start(label+"-startbackup", nativeCommand(h.ctx, "/usr/bin/tmutil", "startbackup", "--block", "--destination", h.destination))
	h.event("time-machine-start", map[string]any{"label": label})
}

func (h *harness) completeBackup(label string) (time.Time, error) {
	deadline := time.Now().Add(90 * time.Minute)
	next := time.Time{}
	for !h.backup.exited() {
		if time.Now().After(deadline) {
			h.t.Fatal("full backup exceeded 90 minute stage budget")
		}
		if time.Now().After(next) {
			var stat syscall.Statfs_t
			h.must(syscall.Statfs(h.work, &stat))
			free := stat.Bavail * uint64(stat.Bsize)
			h.event("time-machine-progress", map[string]any{"label": label, "native_status": h.native("/usr/bin/tmutil", "status"), "free_bytes": free})
			if free < 20<<30 {
				h.t.Fatal("free space below 20 GiB")
			}
			next = time.Now().Add(time.Minute)
		}
		h.pause(time.Second)
	}
	p := h.backup
	h.backup = nil
	h.must(p.log.Close())
	if p.err != nil || h.status() {
		return time.Time{}, fmt.Errorf("Time Machine did not complete cleanly: %w", errors.Join(p.err, errors.New("backup client failed or backup still running")))
	}
	h.event("time-machine-command-completed", map[string]any{"label": label})
	// This is only the command boundary. remoteBackup must also find a completed native backup.
	return time.Now().UTC(), nil
}

type receipt struct {
	Snapshot time.Time
	Key      string
}

func (h *harness) metadata(after time.Time, label string) receipt {
	path := filepath.Join(h.local, "state/backup-receipt.json")
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) //nolint:gosec // This is the application's receipt inside the test-owned state directory.
		if err == nil {
			var point receipt
			h.must(json.Unmarshal(data, &point))
			if point.Snapshot.After(after) {
				objects := h.objects("s3-smb/meta/")
				size, ok := objects["s3-smb/"+point.Key]
				if !ok {
					h.t.Fatal("receipt object absent from S3", point.Key)
				}
				h.save(label+"-receipt.json", point, false)
				h.event("native-point-after-completion", map[string]any{"label": label, "receipt": point, "object_bytes": size})
				return point
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			h.must(err)
		}
		h.pause(time.Second)
	}
	h.t.Fatal("no successful native metadata point after Time Machine completion")
	return receipt{}
}

func mountpoints(text string) []string {
	var paths []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "(smbfs") || !strings.Contains(line, "127.0.0.1:1445/TimeMachine on ") {
			continue
		}
		_, tail, ok := strings.Cut(line, " on ")
		if !ok {
			continue
		}
		path, _, ok := strings.Cut(tail, " (")
		if ok {
			paths = append(paths, path)
		}
	}
	return paths
}

func (h *harness) detach(ctx context.Context) error {
	run := func(args ...string) (string, error) { return h.command(ctx, 2*time.Minute, "", args...) }
	text, err := run("/usr/bin/hdiutil", "info", "-plist")
	if err != nil {
		return err
	}
	var info imageInfo
	if _, err = plist.Unmarshal([]byte(text), &info); err != nil {
		return err
	}
	devices := append([]string(nil), h.attachments...)
	for _, image := range info.Images {
		owned := strings.HasPrefix(image.Path, h.share+"/") || strings.Contains(image.Path, "/127.0.0.1/") || strings.Contains(image.Path, "/127.0.0.1:1445/")
		if !strings.HasSuffix(image.Path, ".sparsebundle") || !owned {
			continue
		}
		for _, entry := range image.Entities {
			if entry.Device != "" {
				devices = append(devices, entry.Device)
				break
			}
		}
	}
	mounts, err := run("/sbin/mount")
	if err != nil {
		return err
	}
	// APFS snapshots mounted by listbackups must go before the image.
	for _, line := range strings.Split(mounts, "\n") {
		if !strings.HasPrefix(line, "com.apple.TimeMachine.") || !strings.Contains(line, " on /Volumes/.timemachine/") {
			continue
		}
		_, tail, _ := strings.Cut(line, " on ")
		path, _, _ := strings.Cut(tail, " (")
		if _, err := run("/sbin/umount", path); err != nil {
			if _, err := run("/sbin/umount", "-f", path); err != nil {
				return err
			}
		}
	}
	for index := len(devices) - 1; index >= 0; index-- {
		if err := h.detachDevice(ctx, devices[index]); err != nil {
			return err
		}
	}
	h.attachments = nil
	return h.detachShares(ctx)
}

func (h *harness) detachDevice(ctx context.Context, device string) error {
	run := func(args ...string) (string, error) { return h.command(ctx, 2*time.Minute, "", args...) }
	if _, err := run("/usr/bin/hdiutil", "detach", device); err == nil {
		return nil
	}
	deadline := time.Now().Add(time.Minute)
	for {
		text, err := run("/usr/bin/hdiutil", "info", "-plist")
		if err != nil {
			return err
		}
		if !strings.Contains(text, "<string>"+device+"</string>") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cannot detach %s", device)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		if _, err := run("/usr/bin/hdiutil", "detach", "-force", device); err == nil {
			return nil
		}
	}
}

func (h *harness) detachShares(ctx context.Context) error {
	run := func(args ...string) (string, error) { return h.command(ctx, 2*time.Minute, "", args...) }
	deadline := time.Now().Add(time.Minute)
	for {
		text, err := run("/sbin/mount")
		if err != nil {
			return err
		}
		paths := mountpoints(text)
		if len(paths) == 0 {
			return nil
		}
		late := time.Now().After(deadline)
		for _, path := range paths {
			if _, err := run("/sbin/umount", path); err != nil && late {
				if _, err := run("/sbin/umount", "-f", path); err != nil {
					return err
				}
			}
		}
		if late {
			text, err := run("/sbin/mount")
			if err != nil {
				return err
			}
			if len(mountpoints(text)) != 0 {
				return errors.New("task SMB mount remains")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (h *harness) attach(bundle string, readonly bool) ([]string, []string) {
	args := []string{"/usr/bin/hdiutil", "attach", "-nobrowse", "-plist"}
	if readonly {
		args = append(args, "-readonly")
	}
	output, err := h.command(h.ctx, 5*time.Minute, "", append(args, bundle)...)
	if err != nil {
		h.t.Log("attach failed", err)
		return nil, nil
	}
	var result struct {
		Entities []entity `plist:"system-entities"`
	}
	_, err = plist.Unmarshal([]byte(output), &result)
	h.must(err)
	var devices, volumes []string
	for _, entry := range result.Entities {
		if entry.Device != "" {
			devices = append(devices, entry.Device)
		}
		if entry.Mount != "" {
			volumes = append(volumes, entry.Mount)
		}
	}
	return devices, volumes
}

func (h *harness) remoteBackup(label, identifier string) string {
	bundles, err := filepath.Glob(filepath.Join(h.share, "*.sparsebundle"))
	h.must(err)
	if len(bundles) != 1 {
		h.t.Fatal("expected one real Time Machine sparsebundle", bundles)
	}
	devices, volumes := h.attach(bundles[0], true)
	if len(volumes) != 1 {
		if len(devices) > 0 {
			h.run(2*time.Minute, "/usr/bin/hdiutil", "detach", "-force", devices[0])
		}
		devices, _ = h.attach(bundles[0], false)
		if len(devices) == 0 {
			h.t.Fatal("writable replay attach failed")
		}
		h.run(5*time.Minute, "/usr/bin/hdiutil", "detach", devices[0])
		devices, volumes = h.attach(bundles[0], true)
	}
	if len(devices) == 0 || len(volumes) != 1 {
		h.t.Fatal("unknown Time Machine image volume layout")
	}
	h.attachments = append(h.attachments, devices[0])
	var backups []string
	for _, flags := range [][]string{{"-m"}, {}} {
		output := h.run(10*time.Minute, append([]string{"/usr/bin/tmutil", "listbackups", "-d", volumes[0]}, flags...)...)
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "/") {
				backups = append(backups, line)
			}
		}
		if len(backups) > 0 {
			break
		}
	}
	latest := ""
	if identifier == "" {
		latest = strings.TrimSpace(h.run(10*time.Minute, "/usr/bin/tmutil", "latestbackup", "-d", volumes[0], "-m"))
	}
	selected, err := helpers.SelectBackup(backups, latest, identifier)
	h.must(err)
	info, err := os.Stat(selected)
	h.must(err)
	if !info.IsDir() || strings.HasSuffix(filepath.Base(selected), ".inProgress") {
		h.t.Fatal("not a completed remote backup directory")
	}
	h.native("/usr/bin/hdiutil", "info", "-plist")
	h.native("/sbin/mount")
	h.save(label+"-remote-selection.json", map[string]any{"image": bundles[0], "device": devices[0], "image_volume": volumes[0], "completed_backups": backups, "selected": selected}, false)
	return selected
}

func (h *harness) backupTree(selected string) string {
	volumes, err := os.ReadDir(selected)
	h.must(err)
	var found []string
	for _, volume := range volumes {
		path := filepath.Join(selected, volume.Name(), strings.TrimPrefix(h.proof, "/"))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		h.must(err)
		if info.IsDir() {
			found = append(found, path)
		}
	}
	if len(found) != 1 {
		h.t.Fatal("created tree is not in exactly one backup volume", found)
	}
	return found[0]
}

func (h *harness) restore(selected, reference, name string) helpers.Counts {
	source := h.backupTree(selected)
	if !strings.HasPrefix(source, "/Volumes/") || filepath.Clean(source) == filepath.Clean(h.proof) {
		h.t.Fatal("restore source is not the mounted backup", source)
	}
	output := filepath.Join(h.work, name)
	h.must(absent(output))
	h.event("native-created-tree-restore-start", map[string]any{"source": source})
	h.run(30*time.Minute, "/usr/bin/tmutil", "restore", "-v", source, output)
	manifest := filepath.Join(h.evidence, name+".jsonl")
	counts := h.manifest(output, manifest)
	expected, err := helpers.ReadManifest(reference)
	h.must(err)
	actual, err := helpers.ReadManifest(manifest)
	h.must(err)
	differences, err := helpers.Compare(expected, actual)
	h.must(err)
	h.save(name+"-differences.json", differences, false)
	if len(differences) != 0 {
		h.t.Fatalf("%d created-tree differences; see %s-differences.json", len(differences), name)
	}
	h.event("native-created-tree-restore-verified", map[string]any{"backup": filepath.Base(selected), "counts": counts})
	return counts
}
