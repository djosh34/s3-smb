// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenBackupRemountsVanishedSelection(t *testing.T) {
	backup := filepath.Join(t.TempDir(), "2026-10-04-082758.backup")
	must(t, os.Mkdir(backup, 0o700))
	_, err := os.Stat(backup)
	must(t, err)
	// Selection succeeded, but backupd removed the mount before the first read.
	must(t, os.Remove(backup))
	calls := 0
	file, err := OpenBackup(backup, func() (string, error) {
		calls++
		return backup, os.Mkdir(backup, 0o700)
	})
	must(t, err)
	defer func() { must(t, file.Close()) }()
	if calls != 1 || file.Name() != backup {
		t.Fatal("did not remount the selected backup", calls, file.Name())
	}
	_, err = file.ReadDir(-1)
	must(t, err)
}

func TestOpenBackupKeepsReadableSelection(t *testing.T) {
	backup := t.TempDir()
	file, err := OpenBackup(backup, func() (string, error) {
		t.Fatal("remounted a readable backup")
		return "", nil
	})
	must(t, err)
	must(t, file.Close())
}

func TestOpenBackupErrors(t *testing.T) {
	failure := errors.New("remount failed")
	missing := filepath.Join(t.TempDir(), "missing.backup")
	_, err := OpenBackup(missing, func() (string, error) { return "", failure })
	if !errors.Is(err, failure) {
		t.Fatal("lost remount error", err)
	}
	_, err = OpenBackup(missing, func() (string, error) { return missing, nil })
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("accepted a snapshot that vanished again", err)
	}
	regular := filepath.Join(t.TempDir(), "regular")
	must(t, os.WriteFile(regular, nil, 0o600))
	_, err = OpenBackup(regular, func() (string, error) {
		t.Fatal("remounted a non-directory")
		return "", nil
	})
	if err == nil {
		t.Fatal("accepted a non-directory")
	}
	_, err = OpenBackup(filepath.Join(regular, "child"), func() (string, error) {
		t.Fatal("remounted on an error other than a missing path")
		return "", nil
	})
	if err == nil {
		t.Fatal("accepted an invalid path")
	}
}
