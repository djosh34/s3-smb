// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestManifestCompare(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "nested/empty"), 0o700))
	must(t, os.WriteFile(filepath.Join(root, "nested/file"), []byte("data"), 0o600))
	must(t, os.WriteFile(filepath.Join(root, "zero"), nil, 0o600))
	before, counts, err := Manifest(root)
	must(t, err)
	if counts != (Counts{Entries: 5, Files: 2, Bytes: 4}) {
		t.Fatal(counts)
	}
	// Permissions are not compared; content, missing, extra and replaced paths are.
	must(t, os.Chmod(filepath.Join(root, "zero"), 0o400))
	must(t, os.WriteFile(filepath.Join(root, "nested/file"), []byte("DATA"), 0o600))
	must(t, os.Remove(filepath.Join(root, "nested/empty")))
	must(t, os.Symlink(root, filepath.Join(root, "nested/empty")))
	must(t, os.WriteFile(filepath.Join(root, "extra"), nil, 0o600))
	after, _, err := Manifest(root)
	must(t, err)
	differences, err := Compare(before, after)
	must(t, err)
	if !slices.Equal(differences, []string{"extra", "nested/empty", "nested/file"}) {
		t.Fatal(differences)
	}
	if _, err := Compare(nil, after); err == nil {
		t.Fatal("empty reference accepted")
	}
}

func TestTmutilStatus(t *testing.T) {
	sample := func(bytes string) string { return "Running = 1;\nBackupPhase = Copying;\nbytes = " + bytes + ";" }
	for text, want := range map[string]bool{
		sample("1e6"):   true,
		sample("0"):     false,
		sample("1e999"): false,
		"Running = 0;\nBackupPhase = Copying;\nbytes = 1;":   false,
		"Running = 1;\nBackupPhase = Finishing;\nbytes = 1;": false,
		"Running = 1;\nBackupPhase = Copying;\n":             false,
	} {
		if Copying(text) != want {
			t.Error(text, want)
		}
	}
	if _, err := Running("Running = 2;"); err == nil {
		t.Error("unknown status accepted")
	}
	for _, test := range []struct {
		before, after string
		want          bool
	}{
		{"1", `"134217728"`, true},
		{"1", "134217727", false},
		{"268435456", "268435456", false},
		{"268435456", "3758096384", false},
	} {
		if BandWriteReady(sample(test.before), sample(test.after)) != test.want {
			t.Error(test)
		}
	}
	must(t, CheckExclusion("[Excluded]  /objects", true))
	if CheckExclusion("[Excluded]  /tree", false) == nil {
		t.Error("wrong exclusion accepted")
	}
	before := map[string]int64{"a": 1, "b": 1}
	if CheckRemoteChange(before, map[string]int64{"a": 1}) == nil {
		t.Error("deletion counted as a remote change")
	}
	must(t, CheckRemoteChange(before, map[string]int64{"a": 2, "b": 1}))
}

func TestCopies(t *testing.T) {
	newest := "db/000000000012-000000000040-bbbb"
	objects := map[string]int64{"db/000000000009-000000000099-aaaa": 1, newest: 1, "db/000000000012-000000000039-cccc": 1, "chunks/zzzz": 1}
	if got := NewestCopy(objects); got != newest {
		t.Fatal(got)
	}
	if got := NewestCopy(map[string]int64{"chunks/a": 1}); got != "" {
		t.Fatal(got)
	}
	log := `{"msg":"SMB serving"}` + "\n" + `{"level":"WARN","msg":"restoring the newest database copy","copy":"` + newest + `"}` + "\nnot json\n"
	if got := RestoredCopy(log); got != newest {
		t.Fatal(got)
	}
	if got := RestoredCopy(`{"msg":"keeping the local database","copy":"` + newest + `"}`); got != "" {
		t.Fatal(got)
	}
	log += `{"msg":"database copy landed","copy":"db/1"}` + "\n" + `{"msg":"database copy upload failed; retrying the same copy","copy":"db/2"}` + "\n" + `{"msg":"database copy landed","copy":"db/2"}` + "\n"
	if got := LandedCopies(log); !slices.Equal(got, []string{"db/1", "db/2"}) {
		t.Fatal(got)
	}
}

func TestStorage(t *testing.T) {
	objects := map[string]int64{"chunks/a": 8, "chunks/b": 3, "chunks/c": 1}
	if got := Bytes(objects, map[string]bool{"chunks/a": true, "chunks/c": true, "chunks/gone": true}); got != 9 {
		t.Fatal(got)
	}
	region, err := B2Region("https://s3.eu-central-003.backblazeb2.com")
	if region != "eu-central-003" || err != nil {
		t.Fatal(region, err)
	}
	for _, endpoint := range []string{"", "http://s3.eu-central-003.backblazeb2.com", "https://s3..backblazeb2.com", "https://s3.eu-central-003.example.com"} {
		if _, err := B2Region(endpoint); err == nil {
			t.Error("accepted", endpoint)
		}
	}
	block := append(append([]byte("random"), Marker...), "more"...)
	if !HoldsMarker(block) || HoldsMarker(Marker[1:]) {
		t.Fatal("marker misread")
	}
}

func TestSelectBackup(t *testing.T) {
	path := "/Volumes/.timemachine/id/2099.backup/2099.backup"
	selected, err := SelectBackup([]string{"/other.backup", path}, "", "2099.backup")
	must(t, err)
	if selected != path {
		t.Fatal(selected)
	}
	for _, test := range []struct {
		latest  string
		backups []string
	}{
		{path, nil},
		{path, []string{path, path}},
		{path + ".inProgress", []string{path + ".inProgress"}},
	} {
		if _, err := SelectBackup(test.backups, test.latest, ""); err == nil {
			t.Error("accepted", test)
		}
	}
}

func TestLaunchdOutput(t *testing.T) {
	pid, err := LaunchdPID("state = running\n\tpid = 123\n")
	if pid != 123 || err != nil {
		t.Fatal(pid, err)
	}
	if pid, err := LaunchdPID("state = waiting\n"); pid != 0 || err != nil {
		t.Fatal(pid, err)
	}
	serving := `{"msg":"SMB serving"}` + "\n"
	if LaunchdServing(serving, 2) || !LaunchdServing(serving, 1) {
		t.Fatal("serving entries miscounted")
	}
}

func TestSMBMountpoints(t *testing.T) {
	text := `//timemachine@127.0.0.1:1445/TimeMachine on /old (smbfs, nodev)
//timemachine@127.0.0.1:54321/TimeMachine on /proxy share (smbfs, nodev)
//timemachine@127.0.0.1:54321/Other on /other (smbfs, nodev)
//timemachine@127.0.0.1:54321/TimeMachine on /wrong-type (apfs)`
	if got := SMBMountpoints(text, "127.0.0.1:54321"); !slices.Equal(got, []string{"/proxy share"}) {
		t.Fatal(got)
	}
}
