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
	output := h.run(2*time.Minute, "/usr/bin/tmutil", "setdestination", "smb://timemachine:synthetic-tm-control@127.0.0.1:1445/TimeMachine")
	if strings.Contains(output, "The backup destination could not be set.") {
		h.t.Fatal("tmutil could not set the destination despite exit 0")
	}
	var info struct {
		Destinations []struct {
			ID string `plist:"ID"`
		} `plist:"Destinations"`
	}
	_, err := plist.Unmarshal([]byte(h.run(2*time.Minute, "/usr/bin/tmutil", "destinationinfo", "-X")), &info)
	h.must(err)
	h.save("destination.json", info)
	if len(info.Destinations) != 1 || info.Destinations[0].ID == "" {
		h.t.Fatal("expected exactly one Time Machine destination with an ID")
	}
	h.destination = info.Destinations[0].ID
	// Port 1445 bypasses Apple's loopback restriction. backupd needs the System keychain.
	keychain := "/Library/Keychains/System.keychain"
	output, err = h.try(2*time.Minute, "/usr/bin/security", "find-internet-password", "-s", "127.0.0.1", "-a", "timemachine", keychain)
	h.t.Log("existing synthetic credential", output, err)
	attributes := []string{"-s", "127.0.0.1", "-a", "timemachine", "-P", "1445", "-r", "smb ", "-p", "TimeMachine"}
	args := append([]string{"/usr/bin/security", "add-internet-password", "-U"}, attributes...)
	args = append(args, "-T", "/System/Library/CoreServices/NetAuthAgent.app/Contents/MacOS/NetAuthSysAgent", "-T", "/System/Library/CoreServices/TimeMachine/backupd", "-w", "synthetic-tm-control", keychain)
	h.run(2*time.Minute, args...)
	h.run(2*time.Minute, append(append([]string{"/usr/bin/security", "find-internet-password"}, attributes...), keychain)...)
}

func (h *harness) status() bool {
	running, err := helpers.Running(h.run(2*time.Minute, "/usr/bin/tmutil", "status"))
	h.must(err)
	return running
}

func (h *harness) startBackup(label string) {
	if h.backup != nil {
		h.t.Fatal("backup already active")
	}
	h.backup = h.start(label+"-startbackup", "/usr/bin/tmutil", "startbackup", "--block", "--destination", h.destination)
	h.t.Log("time-machine-start", label)
}

func (h *harness) completeBackup(label string) (time.Time, error) {
	deadline, next := time.Now().Add(90*time.Minute), time.Time{}
	for !h.backup.exited() {
		if time.Now().After(deadline) {
			h.t.Fatal("full backup exceeded 90 minute stage budget")
		}
		if time.Now().After(next) {
			var stat syscall.Statfs_t
			h.must(syscall.Statfs(h.work, &stat))
			free := stat.Bavail * uint64(stat.Bsize)
			h.t.Log("time-machine-progress", label, "free_bytes", free, h.run(2*time.Minute, "/usr/bin/tmutil", "status"))
			if free < 20<<30 {
				h.t.Fatal("free space below 20 GiB")
			}
			next = time.Now().Add(time.Minute)
		}
		h.pause(time.Second)
	}
	p := h.backup
	h.backup = nil
	p.cancel()
	h.must(p.log.Close())
	if p.err != nil || h.status() {
		return time.Time{}, errors.Join(errors.New("Time Machine did not complete cleanly"), p.err)
	}
	h.t.Log("time-machine-command-completed", label)
	// A command exit is not enough. remoteBackup must also find a completed native backup.
	return time.Now().UTC(), nil
}

type receipt struct {
	Snapshot time.Time
	Key      string
}

func (h *harness) metadata(after time.Time, label string) receipt {
	var point receipt
	h.must(h.waitFor("metadata point after "+after.Format(time.RFC3339), 15*time.Minute, time.Second, func() (bool, error) {
		data, err := os.ReadFile(filepath.Join(h.local, "state/backup-receipt.json"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(data, &point); err != nil {
			return false, err
		}
		return point.Snapshot.After(after), nil
	}))
	size, exists := h.objects("s3-smb/meta/")["s3-smb/"+point.Key]
	if !exists {
		h.t.Fatal("receipt object absent from S3", point.Key)
	}
	h.save(label+"-receipt.json", point)
	h.t.Log("native-point-after-completion", label, point, "object_bytes", size)
	return point
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

func (h *harness) detach() error {
	text, err := h.try(2*time.Minute, "/usr/bin/hdiutil", "info", "-plist")
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
	mounts, err := h.try(2*time.Minute, "/sbin/mount")
	if err != nil {
		return err
	}
	// listbackups mounts APFS snapshots. They must go before their image.
	for _, line := range strings.Split(mounts, "\n") {
		if !strings.HasPrefix(line, "com.apple.TimeMachine.") || !strings.Contains(line, " on /Volumes/.timemachine/") {
			continue
		}
		_, tail, _ := strings.Cut(line, " on ")
		path, _, _ := strings.Cut(tail, " (")
		if _, err := h.try(2*time.Minute, "/sbin/umount", path); err != nil {
			if _, err := h.try(2*time.Minute, "/sbin/umount", "-f", path); err != nil {
				return err
			}
		}
	}
	for index := len(devices) - 1; index >= 0; index-- {
		if err := h.detachDevice(devices[index]); err != nil {
			return err
		}
	}
	h.attachments = nil
	return h.detachShares()
}

func (h *harness) detachDevice(device string) error {
	if _, err := h.try(2*time.Minute, "/usr/bin/hdiutil", "detach", device); err == nil {
		return nil
	}
	return h.waitFor("eject "+device, time.Minute, 5*time.Second, func() (bool, error) {
		text, err := h.try(2*time.Minute, "/usr/bin/hdiutil", "info", "-plist")
		if err != nil {
			return false, err
		}
		if !strings.Contains(text, "<string>"+device+"</string>") {
			return true, nil
		}
		_, err = h.try(2*time.Minute, "/usr/bin/hdiutil", "detach", "-force", device)
		if err != nil {
			h.t.Log("image still busy", device, err)
		}
		return err == nil, nil
	})
}

func (h *harness) detachShares() error {
	err := h.waitFor("SMB unmount", time.Minute, 5*time.Second, func() (bool, error) {
		text, err := h.try(2*time.Minute, "/sbin/mount")
		if err != nil {
			return false, err
		}
		paths := mountpoints(text)
		for _, path := range paths {
			if _, err := h.try(2*time.Minute, "/sbin/umount", path); err != nil {
				h.t.Log("SMB mount still busy", err)
			}
		}
		return len(paths) == 0, nil
	})
	if err == nil || h.ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	text, err := h.try(2*time.Minute, "/sbin/mount")
	if err != nil {
		return err
	}
	for _, path := range mountpoints(text) {
		if _, err = h.try(2*time.Minute, "/sbin/umount", "-f", path); err != nil {
			return err
		}
	}
	text, err = h.try(2*time.Minute, "/sbin/mount")
	if err != nil {
		return err
	}
	if len(mountpoints(text)) != 0 {
		return errors.New("task SMB mount remains")
	}
	return nil
}

func (h *harness) attach(bundle string, readonly bool) ([]string, []string) {
	args := []string{"/usr/bin/hdiutil", "attach", "-nobrowse", "-plist"}
	if readonly {
		args = append(args, "-readonly")
	}
	output, err := h.try(5*time.Minute, append(args, bundle)...)
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
	if !info.IsDir() {
		h.t.Fatal("not a completed remote backup directory")
	}
	h.run(2*time.Minute, "/usr/bin/hdiutil", "info", "-plist")
	h.run(2*time.Minute, "/sbin/mount")
	h.save(label+"-remote-selection.json", map[string]any{"image": bundles[0], "device": devices[0], "image_volume": volumes[0], "completed_backups": backups, "selected": selected})
	return selected
}

func (h *harness) backupTree(selected string) string {
	path, err := helpers.BackupTree(selected, strings.TrimPrefix(h.proof, "/"))
	h.must(err)
	return path
}

func (h *harness) restore(selected, reference, name string) helpers.Counts {
	source := h.backupTree(selected)
	if !strings.HasPrefix(source, "/Volumes/") || filepath.Clean(source) == filepath.Clean(h.proof) {
		h.t.Fatal("restore source is not the mounted backup", source)
	}
	output := filepath.Join(h.work, name)
	h.must(absent(output))
	h.t.Log("native-created-tree-restore-start", source)
	h.run(30*time.Minute, "/usr/bin/tmutil", "restore", "-v", source, output)
	manifest := filepath.Join(h.evidence, name+".json")
	counts := h.manifest(output, manifest)
	expected, err := helpers.ReadManifest(reference)
	h.must(err)
	actual, err := helpers.ReadManifest(manifest)
	h.must(err)
	differences, err := helpers.Compare(expected, actual)
	h.must(err)
	h.save(name+"-differences.json", differences)
	if len(differences) != 0 {
		h.t.Fatalf("%d created-tree differences; see %s-differences.json", len(differences), name)
	}
	h.t.Log("native-created-tree-restore-verified", filepath.Base(selected), counts)
	return counts
}
