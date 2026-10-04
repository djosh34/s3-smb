// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// OpenBackup holds a backup directory open so normal macOS unmounts see it as busy.
// Let pending unmounts settle, then check the path. Remount only the selected completed backup.
func OpenBackup(backup string, list func() (string, error), settle func() error, log func(string)) (*os.File, error) {
	file, err := openBackup(backup, settle, log)
	if !errors.Is(err, os.ErrNotExist) {
		return file, err
	}
	output, err := list()
	if err != nil {
		return nil, err
	}
	selected, err := SelectBackup(strings.Split(strings.TrimSpace(output), "\n"), "", filepath.Base(backup))
	if err != nil {
		return nil, err
	}
	return openBackup(selected, settle, log)
}

func openBackup(backup string, settle func() error, log func(string)) (*os.File, error) {
	file, err := os.Open(backup) //nolint:gosec // The harness supplies a selected native backup path, not user input.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log("selected backup vanished before open; remounting")
		}
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.IsDir() {
		err = errors.New("not a completed remote backup directory")
	}
	if err == nil {
		err = settle()
	}
	if err == nil {
		// An open handle can survive a forced unmount. Check the path the restore will read.
		info, err = os.Stat(backup)
		if errors.Is(err, os.ErrNotExist) {
			log("selected backup vanished after open; remounting")
		}
		if err == nil && !info.IsDir() {
			err = errors.New("selected backup path is no longer a directory")
		}
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
