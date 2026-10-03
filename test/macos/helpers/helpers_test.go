// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
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
	for _, change := range []string{"none", "content", "missing", "extra", "empty", "type", "symlink", "metadata", "outside"} {
		t.Run(change, func(t *testing.T) {
			base := t.TempDir()
			tree := filepath.Join(base, "tree")
			for _, path := range []string{"nested/deeper", "nested/empty", "empty"} {
				must(t, os.MkdirAll(filepath.Join(tree, path), 0o700))
			}
			for path, value := range map[string]string{"file": "independent test data\x00", "nested/deeper/file": "nested test data", "zero": ""} {
				must(t, os.WriteFile(filepath.Join(tree, path), []byte(value), 0o600))
			}
			before, counts, err := Manifest(tree)
			must(t, err)
			if counts.Entries != 8 || counts.Files != 3 || counts.Bytes != 37 {
				t.Fatalf("counts: %+v", counts)
			}
			if before[0].Path != "." || before[0].Type != "directory" {
				t.Fatal(before)
			}
			switch change {
			case "none":
			case "content":
				must(t, os.WriteFile(filepath.Join(tree, "nested/deeper/file"), []byte("changed"), 0o600))
			case "missing":
				must(t, os.Remove(filepath.Join(tree, "file")))
			case "extra":
				must(t, os.WriteFile(filepath.Join(tree, "extra"), []byte("extra"), 0o600))
			case "empty":
				must(t, os.Remove(filepath.Join(tree, "nested/empty")))
			case "type":
				must(t, os.Remove(filepath.Join(tree, "empty")))
				must(t, os.WriteFile(filepath.Join(tree, "empty"), nil, 0o600))
			case "symlink":
				must(t, os.Remove(filepath.Join(tree, "empty")))
				must(t, os.Symlink(base, filepath.Join(tree, "empty")))
			case "metadata":
				must(t, os.Chmod(filepath.Join(tree, "file"), 0o400))
			case "outside":
				must(t, os.WriteFile(filepath.Join(base, "ordinary-system-file"), []byte("not test data"), 0o600))
			}
			after, _, err := Manifest(tree)
			must(t, err)
			slices.Reverse(after)
			path := filepath.Join(base, "manifest.jsonl")
			must(t, WriteManifest(path, after))
			loaded, err := ReadManifest(path)
			must(t, err)
			diff, err := Compare(before, loaded)
			must(t, err)
			equal := change == "none" || change == "metadata" || change == "outside"
			if (len(diff) == 0) != equal {
				t.Fatalf("differences: %+v", diff)
			}
			if change == "content" && (len(diff) != 1 || diff[0].Path != "nested/deeper/file") {
				t.Fatal(diff)
			}
			if change == "symlink" && (len(diff) != 1 || !strings.HasPrefix(diff[0].Actual.Type, "unexpected:")) {
				t.Fatal(diff)
			}
			if err := WriteManifest(path, after); err == nil {
				t.Fatal("overwrote evidence")
			}
		})
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
	path := filepath.Join(t.TempDir(), "bad")
	must(t, os.WriteFile(path, []byte("not json"), 0o600))
	if _, err := ReadManifest(path); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	// A regular /proc file whose read fails checks that hashing does not hide read errors on Linux.
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
	before := map[string]int64{"a": 1}
	for _, after := range []map[string]int64{nil, {}, {"a": 1}} {
		if err := CheckRemoteChange(before, after); err == nil {
			t.Fatal("no remote change", after)
		}
	}
	for _, after := range []map[string]int64{{"a": 2}, {"b": 1}, {"a": 1, "b": 1}} {
		must(t, CheckRemoteChange(before, after))
	}
}

func TestBuildInputs(t *testing.T) {
	for server, expected := range map[string]string{"default": "", "smbnext": "smbnext"} {
		tags, err := BuildTags(server)
		must(t, err)
		if tags != expected {
			t.Fatal(tags)
		}
	}
	if _, err := BuildTags("other"); err == nil {
		t.Fatal("invalid server accepted")
	}
	commit := strings.Repeat("b", 40)
	release, got, err := MinIOPin("ARG MINIO_RELEASE=RELEASE.2099-01-02T03-04-05Z\nARG MINIO_COMMIT=" + commit + "\n")
	must(t, err)
	if got != commit || release != "RELEASE.2099-01-02T03-04-05Z" {
		t.Fatal(release, got)
	}
	for _, source := range []string{"", "ARG MINIO_RELEASE=bad\nARG MINIO_COMMIT=bad", "ARG MINIO_RELEASE=RELEASE.2099-01-02T03-04-05Z\nARG MINIO_COMMIT=" + strings.Repeat("b", 39)} {
		if _, _, err := MinIOPin(source); err == nil {
			t.Fatal("invalid pin accepted")
		}
	}
}
