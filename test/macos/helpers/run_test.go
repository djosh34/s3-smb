// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunBootstrap(t *testing.T) {
	for _, phase := range []string{"scenario", "m4"} {
		t.Run(phase, func(t *testing.T) { checkRunBootstrap(t, phase) })
	}
}

func checkRunBootstrap(t *testing.T, phase string) {
	t.Helper()
	root := t.TempDir()
	// Mirror sudo's clean environment without requiring root or invoking Time Machine.
	for name, script := range map[string]string{
		"sudo": "#!/bin/bash\nshift\nexec /usr/bin/env -i \"$@\"\n",
		"go":   "#!/bin/bash\nprintf 'GOMAXPROCS=%s GOFLAGS=%s\\n' \"${GOMAXPROCS-unset}\" \"${GOFLAGS-unset}\"\nprintf '%s\\n' \"$@\"\n",
	} {
		must(t, os.WriteFile(filepath.Join(root, name), []byte(script), 0o700)) //nolint:gosec // The test-owned mock commands must be executable.
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "../run.sh")
	cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+root, "RUNNER_TEMP="+root, "MAC_WORK="+root, "MAC_ARTIFACTS="+root, "MAC_TRANSFER="+root, "MAC_SERVER=default", "MAC_PHASE="+phase, "GOMAXPROCS=2", "GOFLAGS=-p=2")
	output, err := cmd.CombinedOutput()
	must(t, err)
	text := string(output)
	if !strings.HasPrefix(text, "GOMAXPROCS=unset GOFLAGS=unset\n") {
		t.Fatal("runtime parallelism leaked", text)
	}
	timeout := "110m"
	if phase == "m4" {
		timeout = "140m"
	}
	if !strings.Contains(text, "test\n-p\n1\n") || !strings.Contains(text, "-timeout\n"+timeout+"\n") || !strings.Contains(text, "./test/macos/...\n") {
		t.Fatal("wrong test invocation", text)
	}
	evidence, err := os.ReadFile(filepath.Join(root, "mac-harness.log")) //nolint:gosec // This fixed filename is inside the test-owned temporary directory.
	must(t, err)
	if string(evidence) != text {
		t.Fatal("harness log was not teed", string(evidence))
	}
}
