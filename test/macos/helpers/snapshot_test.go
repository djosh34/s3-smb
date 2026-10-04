// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func settled() error            { return nil }
func discardSnapshotLog(string) {}

func TestOpenBackupRemountsVanishedSelection(t *testing.T) {
	for _, afterOpen := range []bool{false, true} {
		t.Run(map[bool]string{false: "before open", true: "after open"}[afterOpen], func(t *testing.T) {
			backup := filepath.Join(t.TempDir(), "2026-10-04-082758.backup")
			must(t, os.Mkdir(backup, 0o700))
			_, err := os.Stat(backup)
			must(t, err)
			// Selection succeeds before the pending unmount removes the path.
			if !afterOpen {
				must(t, os.Remove(backup))
			}
			waits, calls := 0, 0
			var logs []string
			file, err := OpenBackup(backup, func() (string, error) {
				calls++
				return backup + "\n", os.Mkdir(backup, 0o700)
			}, func() error {
				waits++
				if afterOpen && waits == 1 {
					return os.Remove(backup)
				}
				return nil
			}, func(message string) { logs = append(logs, message) })
			must(t, err)
			defer func() { must(t, file.Close()) }()
			if calls != 1 || file.Name() != backup {
				t.Fatal("did not remount the selected backup", calls, file.Name())
			}
			_, err = os.Stat(file.Name())
			must(t, err)
			_, err = file.ReadDir(-1)
			must(t, err)
			reason := "selected backup vanished before open; remounting"
			if afterOpen {
				reason = "selected backup vanished after open; remounting"
			}
			if !slices.Equal(logs, []string{reason}) {
				t.Fatal("missing unmount reason", logs)
			}
		})
	}
}

func TestOpenBackupKeepsReadableSelection(t *testing.T) {
	backup := t.TempDir()
	waits := 0
	file, err := OpenBackup(backup, func() (string, error) {
		t.Fatal("remounted a readable backup")
		return "", nil
	}, func() error { waits++; return nil }, discardSnapshotLog)
	must(t, err)
	must(t, file.Close())
	if waits != 1 {
		t.Fatal("did not wait for pending unmounts", waits)
	}
}

func TestOpenBackupRejectsOtherBackups(t *testing.T) {
	for _, name := range []string{"different.backup", "selected.backup.inProgress"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			other := filepath.Join(root, name)
			must(t, os.Mkdir(other, 0o700))
			file, err := OpenBackup(filepath.Join(root, "selected.backup"), func() (string, error) {
				return other, nil
			}, func() error {
				t.Fatal("opened an unselected or incomplete backup")
				return nil
			}, discardSnapshotLog)
			if err == nil || file != nil {
				t.Fatal("accepted an unselected or incomplete backup", file, err)
			}
		})
	}
}

func TestOpenBackupErrors(t *testing.T) {
	failure := errors.New("remount failed")
	missing := filepath.Join(t.TempDir(), "missing.backup")
	_, err := OpenBackup(missing, func() (string, error) { return "", failure }, settled, discardSnapshotLog)
	if !errors.Is(err, failure) {
		t.Fatal("lost remount error", err)
	}
	_, err = OpenBackup(missing, func() (string, error) { return missing, nil }, settled, discardSnapshotLog)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("accepted a snapshot that vanished again", err)
	}
	regular := filepath.Join(t.TempDir(), "regular")
	must(t, os.WriteFile(regular, nil, 0o600))
	for _, path := range []string{regular, filepath.Join(regular, "child")} {
		_, err = OpenBackup(path, func() (string, error) {
			t.Fatal("remounted on an error other than a missing path")
			return "", nil
		}, settled, discardSnapshotLog)
		if err == nil {
			t.Fatal("accepted an invalid path", path)
		}
	}
	_, err = OpenBackup(t.TempDir(), func() (string, error) {
		t.Fatal("remounted after cancellation")
		return "", nil
	}, func() error { return context.Canceled }, discardSnapshotLog)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("lost wait cancellation", err)
	}
}

func TestOpenBackupRejectsChangedPath(t *testing.T) {
	backup := filepath.Join(t.TempDir(), "selected.backup")
	must(t, os.Mkdir(backup, 0o700))
	file, err := OpenBackup(backup, func() (string, error) {
		t.Fatal("remounted a replaced non-directory")
		return "", nil
	}, func() error {
		if err := os.Remove(backup); err != nil {
			return err
		}
		return os.WriteFile(backup, nil, 0o600)
	}, discardSnapshotLog)
	if file != nil || err == nil {
		t.Fatal("accepted a backup path that became a file", file, err)
	}
}

func TestOpenBackupDetectsLossAfterRemount(t *testing.T) {
	backup := filepath.Join(t.TempDir(), "selected.backup")
	file, err := OpenBackup(backup, func() (string, error) {
		return backup, os.Mkdir(backup, 0o700)
	}, func() error { return os.Remove(backup) }, discardSnapshotLog)
	if file != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("accepted a backup that vanished after remount", file, err)
	}
}
