// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"debug/buildinfo"
	_ "embed"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// smbtortureTests lists the smbtorture tests the new server must pass.
//
//go:embed smbtorture.allowlist
var smbtortureTests string

// TestSambaInterop connects with Samba's smbclient over encrypted SMB 3.1.1
// and runs the listed smbtorture tests. test/run-linux.sh runs it against a
// race build of the new server.
func TestSambaInterop(t *testing.T) {
	if os.Getenv("S3_SMB_SAMBA") == "" {
		t.Skip("needs Samba and the new server: run scripts/check.sh")
	}
	requireRaceSmbnextBuild(t)
	f := newFixture(t, false)
	f.start()
	auth := t.TempDir() + "/samba.auth"
	if err := os.WriteFile(auth, []byte("username = backup\npassword = "+f.password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	host, port, err := net.SplitHostPort(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"//" + host + "/TimeMachine", "-p", port, "-A", auth,
		"--use-kerberos=off", "--client-protection=encrypt",
		"--option=client min protocol=SMB3_11", "--option=client max protocol=SMB3_11",
	}
	// quit authenticates and connects to the share, then sends no file requests.
	samba(t, exec.CommandContext(ctx, "smbclient"), append(args, "-c", "quit")...)
	for line := range strings.Lines(smbtortureTests) {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		output := samba(t, exec.CommandContext(ctx, "smbtorture"), append(args, "--fullname", "--format=subunit", name)...)
		// A skipped test or a name that matches nothing does not pass.
		if !slices.Contains(strings.Split(output, "\n"), "success: "+name) {
			t.Errorf("smbtorture %s did not report success", name)
		}
	}
}

// requireRaceSmbnextBuild checks that the daemon under test is the new server
// built with the race detector, so the old server cannot pass by mistake.
func requireRaceSmbnextBuild(t *testing.T) {
	t.Helper()
	info, err := buildinfo.ReadFile(daemonBinary)
	if err != nil {
		t.Fatal(err)
	}
	var race, tags string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "-race":
			race = setting.Value
		case "-tags":
			tags = setting.Value
		}
	}
	if race != "true" || !slices.Contains(strings.Split(tags, ","), "smbnext") {
		t.Fatalf("Samba checks need a -race -tags smbnext daemon; got -race=%q -tags=%q", race, tags)
	}
}

// samba runs a Samba tool with args in a temporary directory, where
// smbtorture creates its own temporary files.
func samba(t *testing.T, cmd *exec.Cmd, args ...string) string {
	t.Helper()
	cmd.Args = append(cmd.Args, args...)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	t.Logf("%s output:\n%s", cmd.Path, output)
	if err != nil {
		t.Fatalf("%s: %v", cmd.Path, err)
	}
	return string(output)
}
