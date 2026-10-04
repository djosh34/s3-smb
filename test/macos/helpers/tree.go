// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// BackupTree returns the test tree in the one backed-up volume that has it. A
// symlink in place of the tree does not count.
func BackupTree(backup, relative string) (string, error) {
	volumes, err := os.ReadDir(backup)
	if err != nil {
		return "", err
	}
	var found []string
	for _, volume := range volumes {
		path := filepath.Join(backup, volume.Name(), relative)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			found = append(found, path)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("created tree is not in exactly one backup volume: %v", found)
	}
	return found[0], nil
}
