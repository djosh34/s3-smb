package e2e

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestSmbnextSubsetRunner(t *testing.T) {
	commandErr := errors.New("command failed")
	for _, tt := range []struct {
		name, allowlist, output       string
		failListing, failRun, invalid bool
	}{
		{name: "empty"},
		{name: "comments", allowlist: "# Not ready yet\n\n"},
		{name: "one test", allowlist: "TestFirst"},
		{name: "two tests", allowlist: "TestSecond\nTestFirst"},
		{name: "discovery failure", failListing: true, invalid: true},
		{name: "unknown", allowlist: "TestAbsent", invalid: true},
		{name: "prefix", allowlist: "Test", invalid: true},
		{name: "pattern", allowlist: "Test.*", invalid: true},
		{name: "duplicate", allowlist: "TestFirst\nTestFirst", invalid: true},
		{name: "subtest", allowlist: "TestFirst/child", invalid: true},
		{name: "recursive runner", allowlist: subsetRunner, invalid: true},
		{name: "execution failure", allowlist: "TestFirst", failRun: true, invalid: true},
		{name: "skip", allowlist: "TestFirst", output: `{"Action":"skip","Test":"TestFirst"}`, invalid: true},
		{name: "failure report", allowlist: "TestFirst", output: `{"Action":"fail","Test":"TestFirst"}`, invalid: true},
		{name: "nothing ran", allowlist: "TestFirst", output: `{"Action":"pass"}`, invalid: true},
		{name: "wrong test passed", allowlist: "TestFirst", output: `{"Action":"pass","Test":"TestSecond"}`, invalid: true},
		{name: "missing parent success", allowlist: "TestFirst", output: `{"Action":"pass","Test":"TestFirst/child"}`, invalid: true},
		{name: "one success missing", allowlist: "TestFirst\nTestSecond", output: `{"Action":"pass","Test":"TestFirst"}`, invalid: true},
		{name: "malformed output", allowlist: "TestFirst", output: "not JSON", invalid: true},
		{name: "truncated output", allowlist: "TestFirst", output: `{"Action":`, invalid: true},
		{name: "skipped child", allowlist: "TestFirst", output: `{"Action":"skip","Test":"TestFirst/child"}` + "\n" + `{"Action":"pass","Test":"TestFirst"}`, invalid: true},
		{name: "failed child", allowlist: "TestFirst", output: `{"Action":"fail","Test":"TestFirst/child"}` + "\n" + `{"Action":"pass","Test":"TestFirst"}`, invalid: true},
		{name: "child and parent pass", allowlist: "TestFirst", output: `{"Action":"pass","Test":"TestFirst/child"}` + "\n" + `{"Action":"pass","Test":"TestFirst"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls [][]string
			t.Setenv("S3_SMB_CHECK_MODE", "pr")
			run := func(_ context.Context, tool string, args ...string) (string, error) {
				wantTool := "go"
				if len(calls) > 0 {
					wantTool = "env"
				}
				if tool != wantTool {
					t.Fatalf("unexpected tool %s, want %s", tool, wantTool)
				}
				calls = append(calls, args)
				if len(calls) == 1 {
					if tt.failListing {
						return "listing failed", commandErr
					}
					return "TestFirst\nTestSecond\n" + subsetRunner + "\nok example/e2e 0.01s\n", nil
				}
				if tt.failRun {
					return "test failed", commandErr
				}
				if tt.output != "" {
					return tt.output, nil
				}
				return `{"Action":"pass","Test":"TestFirst"}` + "\n" + `{"Action":"pass","Test":"TestSecond"}`, nil
			}
			err := runSmbnextSubset(t.Context(), run, tt.allowlist)
			if mode := os.Getenv("S3_SMB_CHECK_MODE"); mode != "pr" {
				t.Fatalf("subset changed the parent check mode to %q", mode)
			}
			if (err != nil) != tt.invalid {
				t.Fatalf("got %v; invalid=%t", err, tt.invalid)
			}
			if (tt.failListing || tt.failRun) && !errors.Is(err, commandErr) {
				t.Fatalf("lost command error: %v", err)
			}
			if len(calls) == 0 || !reflect.DeepEqual(calls[0], []string{"test", "-list", "^Test", "./test/e2e"}) {
				t.Fatalf("wrong discovery command: %v", calls)
			}
			if len(calls) > 1 {
				pattern := "^(TestFirst)$"
				if tt.allowlist == "TestSecond\nTestFirst" {
					pattern = "^(TestSecond|TestFirst)$"
				} else if tt.allowlist == "TestFirst\nTestSecond" {
					pattern = "^(TestFirst|TestSecond)$"
				}
				want := []string{"S3_SMB_CHECK_MODE=gate", "go", "test", "-race", "-shuffle=on", "-count=1", "-json", "-timeout=30m", "-run", pattern, "./test/e2e"}
				if len(calls) != 2 || !reflect.DeepEqual(calls[1], want) {
					t.Fatalf("execution commands = %v, want %v", calls[1:], want)
				}
			}
			if tt.name == "empty" || tt.name == "comments" || tt.failListing {
				if len(calls) != 1 {
					t.Fatalf("unexpected subset execution: %v", calls)
				}
			}
		})
	}
}
