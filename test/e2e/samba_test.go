// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"debug/buildinfo"
	_ "embed"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// smbtortureTests lists the smbtorture tests the server must pass.
//
//go:embed smbtorture.allowlist
var smbtortureTests string

// TestSambaInterop connects with Samba's smbclient over encrypted SMB 3.1.1
// and runs the listed smbtorture tests against a race build of the server.
func TestSambaInterop(t *testing.T) {
	f := newFixture(t)
	requireRaceBuild(t)
	f.start()
	auth := t.TempDir() + "/samba.auth"
	if err := os.WriteFile(auth, []byte("username = backup\npassword = "+f.password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"//" + host + "/TimeMachine", "-p", port, "-A", auth,
		"--use-kerberos=off", "--client-protection=encrypt",
		"--option=client min protocol=SMB3_11", "--option=client max protocol=SMB3_11",
		// The server lets one client in at a time. Every connection uses the
		// same client GUID, like the connections of one Mac.
		"--option=libsmb:client_guid=4d616300-0000-0000-0000-000000000000",
	}
	// quit authenticates and connects to the share, then sends no file requests.
	if _, err := samba(t, "smbclient", args, "-c", "quit"); err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(smbtortureTests) {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		output, err := samba(t, "smbtorture", args, "--fullname", "--format=subunit", name)
		// A skipped test or a name that matches nothing does not pass.
		if err != nil || !slices.Contains(strings.Split(output, "\n"), "success: "+name) {
			t.Errorf("smbtorture %s did not report success: %v", name, err)
		}
	}
}

// requireRaceBuild checks that the daemon under test is built with the race
// detector.
func requireRaceBuild(t *testing.T) {
	t.Helper()
	info, err := buildinfo.ReadFile(daemonBinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "-race" && setting.Value == "true" {
			return
		}
	}
	t.Fatal("Samba checks need a daemon built with -race")
}

// samba runs smbclient or smbtorture with the connection args and test args
// for at most five minutes in a temporary directory, where smbtorture creates
// its own temporary files, and logs its output.
func samba(t *testing.T, tool string, args []string, test ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	switch tool {
	case "smbclient":
		cmd = exec.CommandContext(ctx, "smbclient")
	case "smbtorture":
		cmd = exec.CommandContext(ctx, "smbtorture")
	default:
		return "", fmt.Errorf("unknown Samba tool %q", tool)
	}
	cmd.Args = append(append(cmd.Args, args...), test...)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	t.Logf("%s output:\n%s", cmd.Path, output)
	if err != nil {
		return string(output), fmt.Errorf("%s: %w", cmd.Path, err)
	}
	return string(output), nil
}
