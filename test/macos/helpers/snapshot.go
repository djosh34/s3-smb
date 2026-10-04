// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"os"
)

// OpenBackup holds a backup directory open so normal macOS unmounts see it as busy.
// If selection raced with an unmount, remount the same completed backup first.
func OpenBackup(backup string, remount func() (string, error)) (*os.File, error) {
	file, err := os.Open(backup) //nolint:gosec // The harness supplies a selected native backup path, not user input.
	if errors.Is(err, os.ErrNotExist) {
		backup, err = remount()
		if err != nil {
			return nil, err
		}
		file, err = os.Open(backup) //nolint:gosec // Remount selects the same completed native backup again.
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.IsDir() {
		err = errors.New("not a completed remote backup directory")
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
