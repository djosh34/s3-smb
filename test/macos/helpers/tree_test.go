// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupTree(t *testing.T) {
	backup := t.TempDir()
	relative := filepath.Join("Users", "runner", "s3-smb-acceptance-proof")
	expected := filepath.Join(backup, "Macintosh HD - Data", relative)
	must(t, os.MkdirAll(expected, 0o700))
	// Time Machine puts a regular checkpoint file next to the backed-up volume directories.
	must(t, os.WriteFile(filepath.Join(backup, ".com.apple.timemachine.checkpoint"), nil, 0o600))
	must(t, os.Mkdir(filepath.Join(backup, "Other volume"), 0o700))
	path, err := BackupTree(backup, relative)
	must(t, err)
	if path != expected {
		t.Fatal(path)
	}
	if _, err := BackupTree(backup, "missing-fixture"); err == nil {
		t.Fatal("accepted an absent tree")
	}
	must(t, os.MkdirAll(filepath.Join(backup, "Second data volume", relative), 0o700))
	if _, err := BackupTree(backup, relative); err == nil {
		t.Fatal("accepted an ambiguous tree")
	}
	must(t, os.RemoveAll(filepath.Join(backup, "Second data volume")))
	must(t, os.Remove(expected))
	must(t, os.Symlink(backup, expected))
	if _, err := BackupTree(backup, relative); err == nil {
		t.Fatal("followed a replaced tree symlink")
	}
}
