// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestLaunchdPlist(t *testing.T) {
	data, err := os.ReadFile("../../../docs/com.s3-smb.plist")
	must(t, err)
	var original map[string]any
	_, err = plist.Unmarshal(data, &original)
	must(t, err)
	if original["KeepAlive"] != true || original["RunAtLoad"] != true || original["Label"] != "com.s3-smb" {
		t.Fatal("shipped job must start at load and restart after exit", original)
	}
	environment, ok := original["EnvironmentVariables"].(map[string]any)
	if !ok || environment["HOME"] != "/Users/YOUR_USER" {
		t.Fatal("shipped job must set HOME explicitly", environment)
	}
	filled, err := LaunchdPlist(data, "/test/bin & name", "/test/config", "/test/work", "/test/evidence")
	must(t, err)
	var job map[string]any
	_, err = plist.Unmarshal(filled, &job)
	must(t, err)
	for key, value := range map[string]any{
		"ProgramArguments":     []any{"/test/bin & name", "-c", "/test/config", "serve"},
		"WorkingDirectory":     "/test/work",
		"StandardOutPath":      "/test/evidence/launchd-out.log",
		"StandardErrorPath":    "/test/evidence/launchd-err.log",
		"EnvironmentVariables": map[string]any{"HOME": "/test/work"},
	} {
		if !reflect.DeepEqual(job[key], value) {
			t.Fatalf("%s: got %v, want %v", key, job[key], value)
		}
		delete(job, key)
		delete(original, key)
	}
	if !reflect.DeepEqual(job, original) || strings.Contains(string(filled), "YOUR_USER") {
		t.Fatal("path filling changed other settings or left sample paths")
	}
	if _, err = LaunchdPlist([]byte("broken"), "", "", "", ""); err == nil {
		t.Fatal("accepted malformed plist")
	}
	for _, args := range []any{nil, "serve", []string{"binary"}, []string{"binary", "--other", "config", "serve"}, []string{"binary", "-c", "config", "help"}} {
		original["ProgramArguments"] = args
		broken, marshalErr := plist.Marshal(original, plist.XMLFormat)
		must(t, marshalErr)
		if _, err = LaunchdPlist(broken, "", "", "", ""); err == nil {
			t.Fatal("accepted unexpected program arguments", args)
		}
	}
	original["ProgramArguments"] = []string{"binary", "-c", "config", "serve"}
	broken, err := plist.Marshal(original, plist.XMLFormat)
	must(t, err)
	if _, err = LaunchdPlist(broken, "", "", "", ""); err == nil {
		t.Fatal("accepted missing launchd environment")
	}
}

func TestLaunchdPID(t *testing.T) {
	for _, test := range []struct {
		text string
		pid  int
		bad  bool
	}{
		{"state = waiting\n", 0, false},
		{"\tpid = 123\n", 123, false},
		{"\tpid = abc\n", 0, true},
		{"pid = 0", 0, true},
		{"pid = -1", 0, true},
	} {
		pid, err := LaunchdPID(test.text)
		if pid != test.pid || (err != nil) != test.bad {
			t.Fatalf("%q: pid %d, error %v", test.text, pid, err)
		}
	}
}

func TestLaunchdServing(t *testing.T) {
	serving := `{"msg":"SMB serving"}` + "\n"
	for _, test := range []struct {
		text       string
		starts     int
		ready, bad bool
	}{
		{"", 1, false, false},
		{serving, 1, true, false},
		{serving, 2, false, false},
		{serving + serving, 2, true, false},
		{serving + "Continue? [yes/no]: ", 1, false, true},
	} {
		ready, err := LaunchdServing(test.text, test.starts)
		if ready != test.ready || (err != nil) != test.bad {
			t.Fatalf("%q: ready %t, error %v", test.text, ready, err)
		}
	}
}

func TestLaunchdScenarioDefault(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/macos.yml")
	must(t, err)
	if !strings.Contains(string(data), `default: '["server-kill-restart", "launchd-kill-restart",`) {
		t.Fatal("launchd scenario must run in default acceptance")
	}
}
