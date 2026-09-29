// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These run the real entry point in an isolated process: no config/network or
// terminal interfaces are replaced by fakes.
func TestEntryPointProcess(t *testing.T) {
	if os.Getenv("S3_SMB_APP_PROCESS_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(Main(os.Args[i+1:], "process-test"))
		}
	}
	os.Exit(99)
}
func cliProcess(t *testing.T, args ...string) ([]byte, []byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestEntryPointProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "S3_SMB_APP_PROCESS_TEST=1")
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	return out.Bytes(), errout.Bytes(), err
}
func TestHelpVersionHaveNoConfigurationSideEffects(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "helper-ran")
	cfg := filepath.Join(dir, "config.yaml")
	// A valid-shaped command source would write marker if resolution ran; help
	// and version must not even attempt parsing this deliberately invalid YAML.
	if err := os.WriteFile(cfg, []byte("invalid: [\ncommand: [touch, "+marker+"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	for _, command := range []string{"help", "version", "--help", "--version"} {
		out, stderr, err := cliProcess(t, "-c", cfg, command)
		if err != nil || len(out) == 0 || len(stderr) != 0 {
			t.Fatalf("%s: stdout=%q stderr=%q err=%v", command, out, stderr, err)
		}
	}
	for _, p := range []string{marker, filepath.Join(dir, "data"), filepath.Join(dir, "cache")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("unexpected side effect at %s", p)
		}
	}
}
func TestJSONOverrideForCommandAndConfigErrors(t *testing.T) {
	for _, args := range [][]string{{"invalid-secret-marker", "--log-format=json"}, {"serve", "--log-format", "json", "-c", filepath.Join(t.TempDir(), "absent")}} {
		stdout, stderr, err := cliProcess(t, args...)
		if err == nil || len(stdout) != 0 {
			t.Fatalf("stdout=%q stderr=%q err=%v", stdout, stderr, err)
		}
		lines := bytes.Split(bytes.TrimSpace(stderr), []byte{'\n'})
		for _, line := range lines {
			var v map[string]any
			if e := json.Unmarshal(line, &v); e != nil {
				t.Fatalf("not JSON: %q", line)
			}
		}
		if bytes.Contains(stderr, []byte("invalid-secret-marker")) {
			t.Fatal("echoed arbitrary command input")
		}
	}
}
func TestHardExitRetainsOSLockUntilProcessTermination(t *testing.T) {
	if dir := os.Getenv("S3_SMB_APP_LOCK_TEST"); dir != "" {
		lock, err := lockState(dir)
		if err != nil {
			os.Exit(98)
		}
		// Keep a live reference; no deferred close runs on the hard exit path.
		defer lock.Close()
		os.Stdout.WriteString("locked\n")
		time.AfterFunc(500*time.Millisecond, hardExit)
		select {}
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHardExitRetainsOSLockUntilProcessTermination$")
	cmd.Env = append(os.Environ(), "S3_SMB_APP_LOCK_TEST="+dir)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	ready, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || ready != "locked\n" {
		t.Fatalf("ready=%q err=%v", ready, err)
	}
	if l, err := lockState(dir); err == nil {
		l.Close()
		t.Fatal("lock released while owner alive")
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("hard exit reported success")
	}
	l, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

func TestConfirmationWithoutControllingTerminal(t *testing.T) {
	if os.Getenv("S3_SMB_APP_PROMPT_TEST") == "1" {
		if err := confirm("must not use redirected stdin"); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfirmationWithoutControllingTerminal$")
	cmd.Env = append(os.Environ(), "S3_SMB_APP_PROMPT_TEST=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = strings.NewReader("yes\n")
	output, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("/dev/tty")) {
		t.Fatalf("output=%s err=%v", output, err)
	}
}
