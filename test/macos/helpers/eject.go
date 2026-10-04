// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import "strings"

// UnmountSnapshots removes remote APFS snapshots before their disk image can be ejected.
func UnmountSnapshots(run func(...string) (string, error)) error {
	mounts, err := run("/sbin/mount")
	if err != nil {
		return err
	}
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
	return nil
}

// Eject performs one image-ejection attempt with snapshot-before-image ordering.
func Eject(run func(...string) (string, error), device string, force bool) error {
	if err := UnmountSnapshots(run); err != nil {
		return err
	}
	args := []string{"/usr/bin/hdiutil", "detach"}
	if force {
		args = append(args, "-force")
	}
	_, err := run(append(args, device)...)
	return err
}
