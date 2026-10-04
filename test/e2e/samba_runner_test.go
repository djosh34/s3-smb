package e2e

import (
	"context"
	"errors"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

func TestParseTortureAllowlist(t *testing.T) {
	listing := "smbtorture " + sambaVersion + "\nsmb2.example.case.first\nsmb2.example.case.second\n"
	for _, tt := range []struct {
		name, input string
		want        []string
		invalid     bool
	}{
		{name: "empty"},
		{name: "comments", input: "# M2 exclusions\n\n  # more detail\n"},
		{name: "exact names", input: "# tests\n smb2.example.case.second \nsmb2.example.case.first\n", want: []string{"smb2.example.case.second", "smb2.example.case.first"}},
		{name: "unknown", input: "smb2.example.case.absent", invalid: true},
		{name: "suite", input: "smb2.example", invalid: true},
		{name: "test case", input: "smb2.example.case", invalid: true},
		{name: "wildcard", input: "smb2.example.*", invalid: true},
		{name: "multiple names", input: "smb2.example.case.first smb2.example.case.second", invalid: true},
		{name: "duplicate", input: "smb2.example.case.first\nsmb2.example.case.first", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTortureAllowlist(tt.input, listing)
			if (err != nil) != tt.invalid || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, %v; want %v, invalid=%t", got, err, tt.want, tt.invalid)
			}
		})
	}
}

func TestRequireRaceSmbnextBuild(t *testing.T) {
	for _, tt := range []struct {
		name, race, tags string
		valid            bool
	}{
		{name: "race-enabled new server", race: "true", tags: "smbnext", valid: true},
		{name: "multiple tags", race: "true", tags: "other,smbnext", valid: true},
		{name: "plain default build"},
		{name: "race-enabled old server", race: "true"},
		{name: "plain new server", tags: "smbnext"},
		{name: "race disabled", race: "false", tags: "smbnext"},
		{name: "tag substring", race: "true", tags: "not-smbnext"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := requireRaceSmbnextBuild([]debug.BuildSetting{
				{Key: "-race", Value: tt.race},
				{Key: "-tags", Value: tt.tags},
			})
			if (err == nil) != tt.valid {
				t.Fatalf("got %v; valid=%t", err, tt.valid)
			}
		})
	}
}

type sambaInvocation struct {
	tool string
	args []string
}

func TestSambaRunner(t *testing.T) {
	const first = "smb2.example.case.first"
	const second = "smb2.example.case.second"
	commandErr := errors.New("command failed")
	for _, tt := range []struct {
		name, failTool, failArg, output, allowlist, addr, clientCommand string
		commandFailure, invalid                                         bool
	}{
		{name: "empty allowlist"},
		{name: "empty root listing", clientCommand: "ls"},
		{name: "listing command failure", clientCommand: "ls", failTool: "smbclient", failArg: "ls", commandFailure: true, invalid: true},
		{name: "listing error with zero exit", clientCommand: "ls", failTool: "smbclient", failArg: "ls", output: "NT_STATUS_NO_SUCH_FILE listing \\*\n", invalid: true},
		{name: "listing error blocks selected tests", clientCommand: "ls", allowlist: first, failTool: "smbclient", failArg: "ls", output: "NT_STATUS_ACCESS_DENIED listing \\*\n", invalid: true},
		{name: "status-like filename", clientCommand: "ls", failTool: "smbclient", failArg: "ls", output: "  NT_STATUS_example  N  0\n"},
		{name: "two exact tests", allowlist: first + "\n" + second},
		{name: "client version failure", failTool: "smbclient", failArg: "--version", commandFailure: true, invalid: true},
		{name: "torture version failure", failTool: "smbtorture", failArg: "--version", commandFailure: true, invalid: true},
		{name: "wrong client version", failTool: "smbclient", failArg: "--version", output: "Version 4.18.0", invalid: true},
		{name: "wrong torture version", failTool: "smbtorture", failArg: "--version", output: "Version 4.18.0", invalid: true},
		{name: "listing failure", failTool: "smbtorture", failArg: "--list", commandFailure: true, invalid: true},
		{name: "unknown test", allowlist: "smb2.absent.test", invalid: true},
		{name: "invalid address", addr: "no-port", invalid: true},
		{name: "client failure", failTool: "smbclient", failArg: "quit", commandFailure: true, invalid: true},
		{name: "listed failure", allowlist: first + "\n" + second, failTool: "smbtorture", failArg: first, commandFailure: true, invalid: true},
		{name: "skipped test", allowlist: first, failTool: "smbtorture", failArg: first, output: "skip: " + first, invalid: true},
		{name: "nothing ran", allowlist: first, failTool: "smbtorture", failArg: first, invalid: true},
		{name: "different test passed", allowlist: first, failTool: "smbtorture", failArg: first, output: "success: " + second, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls []sambaInvocation
			run := func(_ context.Context, tool string, args ...string) (string, error) {
				calls = append(calls, sambaInvocation{tool: tool, args: args})
				last := args[len(args)-1]
				if tool == tt.failTool && last == tt.failArg {
					if tt.commandFailure {
						return tt.output, commandErr
					}
					return tt.output, nil
				}
				if last == "--version" {
					version := "Version " + sambaVersion
					if tool == "smbtorture" {
						version = "smbtorture " + sambaVersion + "\n" + version
					}
					return version + "\n", nil
				}
				if last == "--list" {
					return first + "\n" + second + "\n", nil
				}
				return "success: " + last + "\n", nil
			}
			addr := tt.addr
			if addr == "" {
				addr = "127.0.0.1:1445"
			}
			clientCommand := tt.clientCommand
			if clientCommand == "" {
				clientCommand = "quit"
			}
			err := runSamba(t.Context(), run, addr, "TimeMachine", "/tmp/auth", tt.allowlist, clientCommand)
			if (err != nil) != tt.invalid {
				t.Fatalf("got %v; invalid=%t", err, tt.invalid)
			}
			if tt.commandFailure && !errors.Is(err, commandErr) {
				t.Fatalf("lost command error: %v", err)
			}
			common := []string{"//127.0.0.1/TimeMachine", "-p", "1445", "-A", "/tmp/auth", "--use-kerberos=off", "--client-protection=encrypt", "--option=client min protocol=SMB3_11", "--option=client max protocol=SMB3_11"}
			for _, call := range calls {
				last := call.args[len(call.args)-1]
				if last == "--version" || last == "--list" {
					continue
				}
				want := append(append([]string{}, common...), "-c", clientCommand)
				if call.tool == "smbtorture" {
					want = append(append([]string{}, common...), "--fullname", "--format=subunit", last)
				}
				if !reflect.DeepEqual(call.args, want) {
					t.Fatalf("%s args: got %v, want %v", call.tool, call.args, want)
				}
				if tt.invalid && (strings.HasSuffix(last, ".second") || tt.clientCommand == "ls" && call.tool == "smbtorture") {
					t.Fatal("runner continued after a failed command")
				}
			}
			if !tt.invalid {
				wantCalls := 4 + strings.Count(tt.allowlist, "\n")
				if tt.allowlist != "" {
					wantCalls++
				}
				if len(calls) != wantCalls {
					t.Fatalf("got %d calls, want %d", len(calls), wantCalls)
				}
			}
		})
	}
}
