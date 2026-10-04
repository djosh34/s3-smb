package e2e

import (
	"context"
	"debug/buildinfo"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

//go:embed smbnext.allowlist
var smbnextAllowlist string

const subsetRunner = "TestSmbnextE2ESubset"

func runSmbnextSubset(ctx context.Context, run integrationCommand, allowlist string) error {
	listing, err := run(ctx, "go", "test", "-list", "^Test", "./test/e2e")
	if err != nil {
		return fmt.Errorf("list e2e tests: %w\n%s", err, listing)
	}
	names, err := parseTestAllowlist(allowlist, listing)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	patterns := make([]string, len(names))
	for i, name := range names {
		if name == subsetRunner {
			return errors.New("smbnext subset cannot select its own runner")
		}
		patterns[i] = regexp.QuoteMeta(name)
	}
	pattern := "^(" + strings.Join(patterns, "|") + ")$"
	// MissingData exercises permanent-read retries only in gate mode. Set it
	// for this child, without changing the parent PR check or other suites.
	output, err := run(ctx, "env", "S3_SMB_CHECK_MODE=gate", "go", "test", "-race", "-shuffle=on", "-count=1", "-json", "-timeout=30m", "-run", pattern, "./test/e2e")
	if err != nil {
		return fmt.Errorf("run smbnext e2e subset: %w\n%s", err, output)
	}
	return checkSubsetResults(names, output)
}

func checkSubsetResults(names []string, output string) error {
	passed := make(map[string]bool)
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var event struct {
			Action string
			Test   string
		}
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode e2e results: %w", err)
		}
		for _, name := range names {
			if event.Test != name && !strings.HasPrefix(event.Test, name+"/") {
				continue
			}
			if event.Action == "skip" || event.Action == "fail" {
				return fmt.Errorf("listed e2e test %s reported %s", event.Test, event.Action)
			}
			if event.Test == name && event.Action == "pass" {
				passed[name] = true
			}
		}
	}
	for _, name := range names {
		if !passed[name] {
			return fmt.Errorf("listed e2e test %s did not report success", name)
		}
	}
	return nil
}

func TestSmbnextE2ESubset(t *testing.T) {
	binary := os.Getenv("S3_SMB_E2E_SUBSET_BINARY")
	if binary == "" {
		t.Skip("needs the smbnext daemon in the Linux integration run")
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireRaceSmbnextBuild(info.Settings); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, tool string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, tool, args...)
		// Go runs package tests from test/e2e; discovery and execution use the root.
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "S3_SMB_E2E_BINARY="+binary, "S3_SMB_E2E_SUBSET_BINARY=")
		output, err := cmd.CombinedOutput()
		t.Logf("%s %v:\n%s", tool, args, output)
		return string(output), err
	}
	if err := runSmbnextSubset(t.Context(), run, smbnextAllowlist); err != nil {
		t.Fatal(err)
	}
}
