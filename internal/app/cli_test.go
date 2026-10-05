// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/logging"
)

func runMain(t *testing.T, args ...string) (code int, stdout, stderr []byte) {
	t.Helper()
	t.Cleanup(func() { logging.Install(os.Stderr) })
	var out, errOut bytes.Buffer
	code = Main(args, "test", &out, &errOut, func(int) { t.Error("hard exit") })
	return code, out.Bytes(), errOut.Bytes()
}

func TestArguments(t *testing.T) {
	for _, args := range [][]string{{"-c", "cfg", "serve"}, {"serve", "-c", "cfg"}, {"serve", "-c=cfg"}} {
		a, err := parseArguments(args)
		if err != nil || a.command != "serve" || a.configPath != "cfg" {
			t.Fatalf("%v: %+v %v", args, a, err)
		}
	}
	for _, args := range [][]string{{}, {"init"}, {"serve", "-c"}, {"serve", "serve"}} {
		if _, err := parseArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestHelpAndVersionReadNoConfiguration(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "helper-ran")
	cfg := filepath.Join(dir, "config.yaml")
	// The command would write marker if resolution ran. Help and version must
	// not even parse this invalid YAML.
	if err := os.WriteFile(cfg, []byte("invalid: [\ncommand: [touch, "+marker+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	for _, command := range []string{"help", "version", "--help", "--version"} {
		code, stdout, stderr := runMain(t, "-c", cfg, command)
		if code != 0 || len(stdout) == 0 || len(stderr) != 0 {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", command, code, stdout, stderr)
		}
	}
	for _, p := range []string{marker, filepath.Join(dir, "data"), filepath.Join(dir, "cache")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected side effect at %s: %v", p, err)
		}
	}
}

func TestLogFormatAppliesToEarlyErrors(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	for _, args := range [][]string{{"invalid-secret-marker", "--log-format=json"}, {"serve", "--log-format", "json", "-c", absent}} {
		code, stdout, stderr := runMain(t, args...)
		if code == 0 || len(stdout) != 0 || len(stderr) == 0 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(stderr), []byte{'\n'}) {
			var v map[string]any
			if err := json.Unmarshal(line, &v); err != nil {
				t.Fatalf("not JSON: %q", line)
			}
		}
		if bytes.Contains(stderr, []byte("invalid-secret-marker")) {
			t.Fatal("echoed arbitrary command input")
		}
	}
	if code, _, _ := runMain(t, "serve", "--log-format", "xml"); code != 2 {
		t.Fatalf("invalid log format: code %d", code)
	}
}

func TestConfirmation(t *testing.T) {
	for _, input := range []string{"yes\n", " YES \n", "no\n", "y\n", "yes", "", "yes" + strings.Repeat(" ", 2048) + "\n"} {
		var output bytes.Buffer
		err := confirmOn(strings.NewReader(input), &output, "New dataset")
		want := input == "yes\n" || input == " YES \n"
		if (err == nil) != want {
			t.Errorf("%q: confirmation success=%v want=%v", input, err == nil, want)
		}
		if !strings.Contains(output.String(), "Continue? [yes/no]: ") {
			t.Fatal("missing prompt")
		}
	}
}

// The child runs without a controlling terminal and with "yes" on stdin, which
// confirm must not read.
func TestConfirmationNeedsControllingTerminal(t *testing.T) {
	if os.Getenv("S3_SMB_APP_PROMPT_TEST") == "1" {
		err := confirm("must not use redirected stdin")
		if err == nil || !strings.Contains(err.Error(), "/dev/tty") {
			t.Fatalf("confirmation without a terminal: %v", err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestConfirmationNeedsControllingTerminal$")
	cmd.Env = append(os.Environ(), "S3_SMB_APP_PROMPT_TEST=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = strings.NewReader("yes\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
}

func TestStateLockExcludesAndRetainsInode(t *testing.T) {
	dir := t.TempDir()
	a, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, e := lockState(dir); e == nil {
		t.Fatal(errors.Join(errors.New("second lock succeeded"), b.Close()))
	}
	before, err := os.Stat(filepath.Join(dir, "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(dir, "state.lock"))
	if err = errors.Join(err, b.Close()); err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("lock inode changed")
	}
}

func TestStateLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "state.lock")); err != nil {
		t.Fatal(err)
	}
	if l, err := lockState(dir); err == nil {
		t.Fatal(errors.Join(errors.New("followed symlink"), l.Close()))
	}
	got, err := os.ReadFile(filepath.Clean(target))
	if err != nil || string(got) != "untouched" {
		t.Fatalf("target changed: %q, %v", got, err)
	}
}
