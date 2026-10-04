// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestManifest(t *testing.T) {
	changes := map[string]func(string) error{
		"none": func(string) error { return nil },
		"content": func(root string) error {
			return os.WriteFile(filepath.Join(root, "nested/deeper/file"), []byte("changed"), 0o600)
		},
		"missing": func(root string) error { return os.Remove(filepath.Join(root, "file")) },
		"extra":   func(root string) error { return os.WriteFile(filepath.Join(root, "extra"), []byte("extra"), 0o600) },
		"empty":   func(root string) error { return os.Remove(filepath.Join(root, "nested/empty")) },
		"type": func(root string) error {
			path := filepath.Join(root, "empty")
			return errors.Join(os.Remove(path), os.WriteFile(path, nil, 0o600))
		},
		"symlink": func(root string) error {
			return errors.Join(os.Remove(filepath.Join(root, "empty")), os.Symlink(filepath.Dir(root), filepath.Join(root, "empty")))
		},
		"metadata": func(root string) error { return os.Chmod(filepath.Join(root, "file"), 0o400) },
		"outside": func(root string) error {
			return os.WriteFile(filepath.Join(filepath.Dir(root), "outside"), []byte("not test data"), 0o600)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) { checkManifest(t, name, change) })
	}
}

func checkManifest(t *testing.T, name string, change func(string) error) {
	root := filepath.Join(t.TempDir(), "tree")
	for _, dir := range []string{"nested/deeper", "nested/empty", "empty"} {
		must(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	for path, value := range map[string]string{"file": "independent test data\x00", "nested/deeper/file": "nested test data", "zero": ""} {
		must(t, os.WriteFile(filepath.Join(root, path), []byte(value), 0o600))
	}
	before, counts, err := Manifest(root)
	must(t, err)
	if counts != (Counts{Entries: 8, Files: 3, Bytes: 38}) {
		t.Fatal(counts)
	}
	entries, err := index(before)
	must(t, err)
	for _, dir := range []string{".", "empty", "nested", "nested/empty", "nested/deeper"} {
		if entries[dir].Type != "directory" {
			t.Fatal(dir, entries[dir])
		}
	}
	if entries["zero"].Type != "file" || entries["zero"].Bytes != 0 || len(entries["zero"].SHA256) != 64 {
		t.Fatal(entries["zero"])
	}
	must(t, change(root))
	after, _, err := Manifest(root)
	must(t, err)
	slices.Reverse(after)
	path := filepath.Join(filepath.Dir(root), "manifest.json")
	must(t, WriteManifest(path, after))
	loaded, err := ReadManifest(path)
	must(t, err)
	differences, err := Compare(before, loaded)
	must(t, err)
	equal := name == "none" || name == "metadata" || name == "outside"
	if (len(differences) == 0) != equal {
		t.Fatal(differences)
	}
	if name == "content" && !slices.Equal(differences, []string{"nested/deeper/file"}) {
		t.Fatal(differences)
	}
	if name == "symlink" {
		entries, err := index(loaded)
		must(t, err)
		if !strings.HasPrefix(entries["empty"].Type, "unexpected:") || len(differences) != 1 {
			t.Fatal(entries, differences)
		}
	}
	if err := WriteManifest(path, after); err == nil {
		t.Fatal("overwrote evidence")
	}
}

func TestManifestErrors(t *testing.T) {
	if _, _, err := Manifest(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("silent walk failure")
	}
	for _, rows := range [][]Entry{nil, {{Path: "."}, {Path: "."}}} {
		if _, err := Compare(rows, []Entry{{Path: "."}}); err == nil {
			t.Fatal("invalid reference accepted")
		}
		if _, err := Compare([]Entry{{Path: "."}}, rows); err == nil {
			t.Fatal("invalid actual accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	must(t, os.WriteFile(path, []byte("not json"), 0o600))
	if _, err := ReadManifest(path); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, err := os.Stat("/proc/self/mem"); err == nil {
		if _, _, err := Manifest("/proc/self/mem"); err == nil {
			t.Fatal("silent read failure")
		}
	}
}

func TestChecks(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		output := "[Included]  /tree"
		if excluded {
			output = "[Excluded]  /objects"
		}
		must(t, CheckExclusion(output, excluded))
		if err := CheckExclusion(output, !excluded); err == nil {
			t.Fatal("wrong exclusion accepted")
		}
	}
	for _, path := range Exclusions {
		if path == "/Users" || path == "/Users/runner" || path == "/System/Volumes/Data" || strings.ContainsAny(path, "\r\n") {
			t.Fatal(path)
		}
	}
	if len(Exclusions) != 45 {
		t.Fatal("changed reviewed list", len(Exclusions))
	}
	for _, text := range []string{"", "Running = 2;"} {
		if _, err := Running(text); err == nil {
			t.Fatal(text)
		}
	}
	for _, number := range []string{"1", "1.5", "1e6", "\"123\""} {
		if !Copying("Running = 1;\nBackupPhase = Copying;\nbytes = " + number + ";") {
			t.Fatal(number)
		}
	}
	for _, text := range []string{"Running = 0;\nBackupPhase = Copying;\nbytes = 1;", "Running = 1;\nBackupPhase = Finishing;\nbytes = 1;", "Running = 1;\nBackupPhase = Copying;"} {
		if Copying(text) {
			t.Fatal(text)
		}
	}
	for _, number := range []string{"0", "-1", "NaN", "Inf", "1e999"} {
		if Copying("Running = 1;\nBackupPhase = Copying;\nbytes = " + number + ";") {
			t.Fatal(number)
		}
	}
	before := map[string]int64{"a": 1, "b": 1}
	for _, after := range []map[string]int64{nil, {}, {"a": 1, "b": 1}, {"a": 1}} {
		if err := CheckRemoteChange(before, after); err == nil {
			t.Fatal("no remote change", after)
		}
	}
	for _, after := range []map[string]int64{{"a": 2}, {"c": 1}, {"a": 1, "b": 1, "c": 1}} {
		must(t, CheckRemoteChange(before, after))
	}
}

func TestConfirmation(t *testing.T) {
	initialize := "Initialize a genuinely empty S3 dataset?\nContinue? [yes/no]: "
	recoverPrompt := "Recover metadata from meta/dump-2099\nContinue? [yes/no]: "
	point, err := Confirmation("initialize", initialize)
	must(t, err)
	if point != "" {
		t.Fatal(point)
	}
	point, err = Confirmation("recover", recoverPrompt)
	must(t, err)
	if point != "meta/dump-2099" {
		t.Fatal(point)
	}
	for _, test := range []struct{ phase, text string }{{"restart", initialize}, {"initialize", recoverPrompt}, {"recover", initialize}, {"initialize", "Continue? [yes/no]: "}, {"recover", ""}} {
		if _, err := Confirmation(test.phase, test.text); err == nil {
			t.Fatal("unexpected consent accepted", test)
		}
	}
}

func TestSelectBackup(t *testing.T) {
	path := "/Volumes/.timemachine/id/2099.backup/2099.backup"
	for _, identifier := range []string{"", "2099.backup"} {
		selected, err := SelectBackup([]string{path}, path, identifier)
		must(t, err)
		if selected != path {
			t.Fatal(selected)
		}
	}
	for _, test := range []struct {
		latest, identifier string
		paths              []string
	}{
		{path, "", nil},
		{"/source", "", []string{path}},
		{"", "missing.backup", []string{path}},
		{path, "", []string{path, path}},
		{"", "2099.backup", []string{path, path}},
		{"relative", "", []string{"relative"}},
		{"/Volumes/2099.inProgress", "", []string{"/Volumes/2099.inProgress"}},
	} {
		if _, err := SelectBackup(test.paths, test.latest, test.identifier); err == nil {
			t.Fatal(test, "accepted incomplete backup")
		}
	}
}
