//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"context"
	"errors"
	"net"
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
	output := h.run(2*time.Minute, "/usr/bin/tmutil", "setdestination", "smb://timemachine:synthetic-tm-control@"+h.smbAddress+"/TimeMachine")
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
	// A nonstandard port bypasses Apple's loopback restriction. backupd needs the System keychain.
	keychain := "/Library/Keychains/System.keychain"
	output, err = h.try(2*time.Minute, "/usr/bin/security", "find-internet-password", "-s", "127.0.0.1", "-a", "timemachine", keychain)
	h.t.Log("existing synthetic credential", output, err)
	_, port, err := net.SplitHostPort(h.smbAddress)
	h.must(err)
	attributes := []string{"-s", "127.0.0.1", "-a", "timemachine", "-P", port, "-r", "smb ", "-p", "TimeMachine"}
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
		err := readJSON(h.workDir, "daemon/state/backup-receipt.json", &point)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
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

func (h *harness) mountpoints(text string) []string {
	return helpers.SMBMountpoints(text, h.smbAddress)
}

func (h *harness) detach() error {
	if h.backupDirectory != nil {
		root := h.backupDirectory
		h.backupDirectory = nil
		if err := root.Close(); err != nil {
			h.t.Error("close held backup", err)
		}
	}
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
		owned := strings.HasPrefix(image.Path, h.share+"/") || strings.Contains(image.Path, "/127.0.0.1/") || strings.Contains(image.Path, "/"+h.smbAddress+"/")
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
	if len(devices) == 0 {
		if err := h.unmountSnapshots(); err != nil {
			return err
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

// unmountSnapshots unmounts the remote APFS snapshots of Time Machine, which
// keep their disk image busy.
func (h *harness) unmountSnapshots() error {
	mounts, err := h.try(2*time.Minute, "/sbin/mount")
	if err != nil {
		return err
	}
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
	return nil
}

// eject makes one attempt to detach a disk image, after its snapshots.
func (h *harness) eject(device string, force bool) error {
	if err := h.unmountSnapshots(); err != nil {
		return err
	}
	args := []string{"/usr/bin/hdiutil", "detach"}
	if force {
		args = append(args, "-force")
	}
	_, err := h.try(2*time.Minute, append(args, device)...)
	return err
}

func (h *harness) detachDevice(device string) error {
	if err := h.eject(device, false); err == nil {
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
		err = h.eject(device, true)
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
		paths := h.mountpoints(text)
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
	for _, path := range h.mountpoints(text) {
		if _, err = h.try(2*time.Minute, "/sbin/umount", "-f", path); err != nil {
			return err
		}
	}
	text, err = h.try(2*time.Minute, "/sbin/mount")
	if err != nil {
		return err
	}
	if len(h.mountpoints(text)) != 0 {
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
	selected = h.holdBackup(selected, volumes[0])
	h.run(2*time.Minute, "/usr/bin/hdiutil", "info", "-plist")
	h.run(2*time.Minute, "/sbin/mount")
	h.save(label+"-remote-selection.json", map[string]any{"image": bundles[0], "device": devices[0], "image_volume": volumes[0], "completed_backups": backups, "selected": selected})
	return selected
}

// holdBackup keeps the selected backup directory open until detach, so normal
// unmounts see it as busy. macOS can unmount the backup's snapshot before or
// just after the open. Listing the backups mounts it again.
func (h *harness) holdBackup(selected, volume string) string {
	if h.backupDirectory != nil {
		h.t.Fatal("backup directory already held open")
	}
	// Bound both the remount command and retries, not just the pauses between them.
	parent := h.ctx
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	h.ctx = ctx
	defer func() { cancel(); h.ctx = parent }()
	h.must(h.waitFor("open selected backup "+selected, time.Minute, time.Second, func() (bool, error) {
		root, err := h.openBackup(selected)
		if errors.Is(err, os.ErrNotExist) {
			output, listErr := h.try(time.Minute, "/usr/bin/tmutil", "listbackups", "-d", volume, "-m")
			if listErr != nil {
				return false, listErr
			}
			path, selectErr := helpers.SelectBackup(strings.Split(strings.TrimSpace(output), "\n"), "", filepath.Base(selected))
			if selectErr != nil {
				return false, selectErr
			}
			root, err = h.openBackup(path)
			if errors.Is(err, os.ErrNotExist) {
				h.t.Log("selected backup vanished again after remount", path, err)
				return false, nil
			}
		}
		if err != nil {
			return false, err
		}
		h.backupDirectory = root
		return true, nil
	}))
	h.t.Log("selected backup held open until detach", h.backupDirectory.Name())
	return h.backupDirectory.Name()
}

// openBackup opens a backup directory, lets pending unmounts settle, then
// checks the path again. An open directory can survive a forced unmount, but
// the restore reads the path.
func (h *harness) openBackup(path string) (*os.Root, error) {
	root, err := os.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		h.t.Log("selected backup vanished before open; remounting", path)
	}
	if err != nil {
		return nil, err
	}
	h.pause(2 * time.Second)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		h.t.Log("selected backup vanished after open; remounting", path)
	}
	if err == nil && !info.IsDir() {
		err = errors.New("selected backup path is no longer a directory")
	}
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}

func (h *harness) backupTree(selected string) string {
	path, err := helpers.BackupTree(selected, strings.TrimPrefix(h.proof, "/"))
	h.must(err)
	return path
}

// restore restores the test tree from the selected backup and requires it to
// match the expected manifest.
func (h *harness) restore(selected string, expected []helpers.Entry, name string) helpers.Counts {
	source := h.backupTree(selected)
	if !strings.HasPrefix(source, "/Volumes/") || filepath.Clean(source) == filepath.Clean(h.proof) {
		h.t.Fatal("restore source is not the mounted backup", source)
	}
	output := filepath.Join(h.work, name)
	h.must(absent(output))
	h.t.Log("native-created-tree-restore-start", source)
	h.run(30*time.Minute, "/usr/bin/tmutil", "restore", "-v", source, output)
	actual, counts := h.manifest(output, h.evidenceDir, name+".json")
	differences, err := helpers.Compare(expected, actual)
	h.must(err)
	h.save(name+"-differences.json", differences)
	if len(differences) != 0 {
		h.t.Fatalf("%d created-tree differences; see %s-differences.json", len(differences), name)
	}
	h.t.Log("native-created-tree-restore-verified", filepath.Base(selected), counts)
	return counts
}
