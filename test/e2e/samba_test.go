package e2e

import (
	"context"
	"debug/buildinfo"
	_ "embed"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

const sambaVersion = "4.17.12-Debian"

//go:embed smbtorture.allowlist
var tortureAllowlist string

type sambaCommand func(context.Context, string, ...string) (string, error)

// parseTortureAllowlist accepts only individual test IDs from --list, never
// suite selectors or patterns that could silently add tests at a later milestone.
func parseTortureAllowlist(allowlist, listing string) ([]string, error) {
	available := make(map[string]bool)
	for _, line := range strings.Split(listing, "\n") {
		available[strings.TrimSpace(line)] = true
	}
	seen := make(map[string]bool)
	var names []string
	for i, line := range strings.Split(allowlist, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		if strings.ContainsAny(name, "*?[] \t\r") || !available[name] {
			return nil, fmt.Errorf("allowlist line %d is not an exact test ID: %q", i+1, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("allowlist line %d repeats %q", i+1, name)
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

func runSamba(ctx context.Context, run sambaCommand, addr, share, authFile, allowlist string) error {
	for _, tool := range []string{"smbclient", "smbtorture"} {
		output, err := run(ctx, tool, "--version")
		if err != nil {
			return fmt.Errorf("%s version: %w", tool, err)
		}
		want := "Version " + sambaVersion
		if tool == "smbtorture" {
			want = "smbtorture " + sambaVersion + "\n" + want
		}
		if strings.TrimSpace(output) != want {
			return fmt.Errorf("%s version: want %s, got %q", tool, sambaVersion, output)
		}
	}
	listing, err := run(ctx, "smbtorture", "--list")
	if err != nil {
		return fmt.Errorf("smbtorture listing: %w", err)
	}
	names, err := parseTortureAllowlist(allowlist, listing)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("Samba address: %w", err)
	}
	args := []string{
		"//" + host + "/" + share, "-p", port, "-A", authFile,
		"--use-kerberos=off", "--client-protection=encrypt",
		"--option=client min protocol=SMB3_11", "--option=client max protocol=SMB3_11",
	}
	// quit still authenticates and tree-connects, but sends no file requests.
	output, err := run(ctx, "smbclient", append(args, "-c", "quit")...)
	if err != nil {
		return fmt.Errorf("smbclient connect: %w\n%s", err, output)
	}
	for _, name := range names {
		output, err := run(ctx, "smbtorture", append(args, "--fullname", "--format=subunit", name)...)
		if err != nil {
			return fmt.Errorf("smbtorture %s: %w\n%s", name, err, output)
		}
		// A skipped test or a selector that ran nothing is not a passing gate.
		passed := false
		for _, line := range strings.Split(output, "\n") {
			if line == "success: "+name {
				passed = true
			}
		}
		if !passed {
			return fmt.Errorf("smbtorture %s did not report success:\n%s", name, output)
		}
	}
	return nil
}

// The tagged app constructor selects internal/smb/server. Check the executable,
// not the test binary, so a plain or old-server daemon cannot pass the race run.
func requireRaceSmbnextBuild(settings []debug.BuildSetting) error {
	var race, tags string
	for _, setting := range settings {
		switch setting.Key {
		case "-race":
			race = setting.Value
		case "-tags":
			tags = setting.Value
		}
	}
	if race != "true" || !slices.Contains(strings.Split(tags, ","), "smbnext") {
		return fmt.Errorf("Samba daemon must be built with -race -tags smbnext; got -race=%q -tags=%q", race, tags)
	}
	return nil
}

func TestSambaInterop(t *testing.T) {
	binary := os.Getenv("S3_SMB_SAMBA_BINARY")
	if binary == "" {
		t.Skip("needs the smbnext daemon and Samba in the Linux integration run")
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireRaceSmbnextBuild(info.Settings); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, false)
	f.binary = binary
	f.start()
	authFile := t.TempDir() + "/samba.auth"
	if err := os.WriteFile(authFile, []byte("username = backup\npassword = "+f.password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, tool string, args ...string) (string, error) {
		commandCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(commandCtx, tool, args...)
		// smbtorture creates a temporary output directory, so it cannot run in /src.
		cmd.Dir = t.TempDir()
		output, err := cmd.CombinedOutput()
		if len(args) != 1 || args[0] != "--list" {
			t.Logf("%s output:\n%s", tool, output)
		}
		return string(output), err
	}
	if err := runSamba(t.Context(), run, f.addr, "TimeMachine", authFile, tortureAllowlist); err != nil {
		t.Fatal(err)
	}
	t.Log("Samba connected with encryption; the M2 credit allowlist is empty by the decision on #188")
}
