package e2e

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonStartupIgnoresUnrelatedListener(t *testing.T) {
	f := newFixture(t, false)
	unrelated, err := net.Listen("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unrelated.Close(); err != nil {
			t.Error(err)
		}
	})
	occupied := unrelated.Addr().String()
	f.addr = occupied
	d := f.start()
	if f.addr == occupied {
		select {
		case exitErr := <-d.done:
			d.stopped = true
			d.closeLogs()
			t.Fatalf("fixture reported SMB readiness on unrelated listener %s; daemon startup exit: %v", occupied, exitErr)
		case <-time.After(5 * time.Second):
			t.Fatalf("fixture reported SMB readiness on unrelated listener %s", occupied)
		}
	}
	if f.generation != 2 {
		t.Fatalf("forced first collision used %d attempts, want 2", f.generation)
	}
	_, closeShare := f.share()
	closeShare()
}

func TestDaemonStartupFailures(t *testing.T) {
	if mode := os.Getenv("S3_SMB_STARTUP_PROBE"); mode != "" {
		runStartupFailureProbe(t, mode)
		return
	}
	if os.Getenv("S3_SMB_E2E_ENDPOINT") == "" {
		t.Skip("needs the actual daemon and MinIO")
	}
	for _, tt := range []struct {
		name, cause string
		attempts    int
		succeeds    bool
	}{
		{name: "three collisions", cause: "bind: address already in use", attempts: 3},
		{name: "other failure", cause: "read-only mode cannot initialize an empty dataset", attempts: 1},
		{name: "failStart", attempts: 1, succeeds: true},
		{name: "allocator failure", cause: "select SMB port after bind collision: allocator failed", attempts: 1},
		{name: "shared deadline", cause: "SMB startup timeout after bind collision", attempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			artifacts := t.TempDir()
			if root := os.Getenv("S3_SMB_TEST_ARTIFACTS"); root != "" {
				parent := filepath.Join(root, "startup-probes")
				if err := os.MkdirAll(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				var err error
				artifacts, err = os.MkdirTemp(parent, strings.ReplaceAll(tt.name, " ", "-")+"-")
				if err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDaemonStartupFailures$", "-test.v")
			cmd.Env = append(os.Environ(), "S3_SMB_STARTUP_PROBE="+tt.name, "S3_SMB_TEST_ARTIFACTS="+artifacts)
			started := time.Now()
			output, err := cmd.CombinedOutput()
			t.Logf("startup probe:\n%s", output)
			if tt.succeeds {
				if err != nil {
					t.Fatalf("expected failStart to return its stopped daemon: %v", err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(output), tt.cause) {
					t.Fatalf("missing explicit terminal startup cause %q: %v\n%s", tt.cause, err, output)
				}
			}
			if tt.name == "shared deadline" && time.Since(started) > 4*time.Second {
				t.Fatal("startup reset its two-second total test budget")
			}
			records, err := filepath.Glob(filepath.Join(artifacts, "*", "*", "startup.json"))
			if err != nil || len(records) != tt.attempts {
				t.Fatalf("startup recorded %d attempts, want %d: %v", len(records), tt.attempts, err)
			}
			pids := make(map[int]bool)
			for _, path := range records {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var record struct {
					PID     int    `json:"pid"`
					Address string `json:"requested_address"`
				}
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				if record.PID <= 0 || pids[record.PID] || record.Address == "" {
					t.Fatalf("attempt did not record its own PID/address: %s", data)
				}
				pids[record.PID] = true
			}
		})
	}
}

func runStartupFailureProbe(t *testing.T, mode string) {
	t.Helper()
	f := newFixture(t, false)
	if mode == "other failure" {
		f.readonly = true
		f.start()
		t.Fatal("unexpected readiness after read-only startup failure")
	}
	occupied := func() (string, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		t.Cleanup(func() {
			if err := listener.Close(); err != nil {
				t.Error(err)
			}
		})
		return listener.Addr().String(), nil
	}
	var err error
	f.addr, err = occupied()
	if err != nil {
		t.Fatal(err)
	}
	if mode == "failStart" {
		f.failStart = true
		d := f.startWithPorts(occupied)
		if !d.stopped || f.generation != 1 {
			t.Fatal("failStart retried or reported readiness")
		}
		return
	}
	if mode == "allocator failure" {
		f.startWithPorts(func() (string, error) { return "", errors.New("allocator failed") })
	} else if mode == "shared deadline" {
		f.startupTimeout = 2 * time.Second
		f.startWithPorts(func() (string, error) {
			time.Sleep(2 * time.Second)
			return occupied()
		})
	} else {
		f.startWithPorts(occupied)
	}
	t.Fatal("unexpected readiness after bind collisions")
}

func TestDaemonBindCollision(t *testing.T) {
	const address = "127.0.0.1:1445"
	const collision = "listen on configured SMB address (no fallback): listen tcp 127.0.0.1:1445: bind: address already in use"
	for _, tt := range []struct {
		name, message, failure, address string
		matches                         bool
	}{
		{name: "exact", message: "service stopped with failure", failure: collision, address: address, matches: true},
		{name: "different port", message: "service stopped with failure", failure: collision, address: "127.0.0.1:1446"},
		{name: "other message", message: "unrelated diagnostic", failure: collision, address: address},
		{name: "other error", message: "service stopped with failure", failure: "permission denied", address: address},
		{name: "extra error", message: "service stopped with failure", failure: collision + "; cleanup failed", address: address},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output, err := json.Marshal(map[string]string{"msg": tt.message, "error": tt.failure})
			if err != nil {
				t.Fatal(err)
			}
			if got := daemonBindCollision(output, tt.address); got != tt.matches {
				t.Fatalf("collision=%t, want %t", got, tt.matches)
			}
		})
	}
	for _, output := range []string{"", "bind: address already in use", "{\"msg\":"} {
		if daemonBindCollision([]byte(output), address) {
			t.Fatalf("unparsed startup output counted as a collision: %q", output)
		}
	}
}

func TestDaemonListeningAddress(t *testing.T) {
	for _, tt := range []struct {
		name, output, address string
		invalid               bool
	}{
		{name: "empty"},
		{name: "unrelated startup record", output: "{\"msg\":\"Create session\",\"address\":\"127.0.0.1:1445\"}\n"},
		{name: "startup failure", output: "{\"msg\":\"service stopped with failure\",\"error\":\"bind: address already in use\"}\n"},
		{name: "incomplete readiness", output: "{\"msg\":\"SMB serving\",\"address\":\"127.0.0.1:1445\"}"},
		{name: "partial trailing record", output: "{\"msg\":\"Create session\"}\n{\"msg\":\"SMB serving\""},
		{name: "ready", output: "{\"msg\":\"SMB serving\",\"address\":\"127.0.0.1:1445\"}\n", address: "127.0.0.1:1445"},
		{name: "ready before partial record", output: "{\"msg\":\"SMB serving\",\"address\":\"127.0.0.1:1445\"}\n{", address: "127.0.0.1:1445"},
		{name: "blank line", output: "\n {\"msg\":\"SMB serving\",\"address\":\"127.0.0.1:1445\"}\n", address: "127.0.0.1:1445"},
		{name: "malformed complete record", output: "not JSON\n", invalid: true},
		{name: "missing address", output: "{\"msg\":\"SMB serving\"}\n", invalid: true},
		{name: "unbound port", output: "{\"msg\":\"SMB serving\",\"address\":\"127.0.0.1:0\"}\n", invalid: true},
		{name: "invalid port", output: "{\"msg\":\"SMB serving\",\"address\":\"127.0.0.1:not-a-port\"}\n", invalid: true},
		{name: "port overflow", output: "{\"msg\":\"SMB serving\",\"address\":\"127.0.0.1:65536\"}\n", invalid: true},
		{name: "not loopback", output: "{\"msg\":\"SMB serving\",\"address\":\"192.0.2.1:1445\"}\n", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			address, err := daemonListeningAddress([]byte(tt.output))
			if address != tt.address || (err != nil) != tt.invalid {
				t.Fatalf("address=%q, error=%v; want address=%q, invalid=%t", address, err, tt.address, tt.invalid)
			}
		})
	}
}
