// SPDX-License-Identifier: AGPL-3.0-only
package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	smithylog "github.com/aws/smithy-go/logging"
	"github.com/sirupsen/logrus"
)

func records(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	scan := bufio.NewScanner(bytes.NewReader(b))
	for scan.Scan() {
		var r map[string]any
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			t.Fatalf("invalid JSON frame: %q: %v", scan.Text(), err)
		}
		out = append(out, r)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestFormatsLevelsAndRedaction(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		for _, level := range []string{"debug", "info", "warn", "error"} {
			t.Run(format+"/"+level, func(t *testing.T) {
				var out bytes.Buffer
				Install(&out)
				if err := Configure(format, level); err != nil {
					t.Fatal(err)
				}
				marker := "SYNTHETIC_secret/with space\nnewline_91"
				RegisterSecret(marker)
				l := slog.With("bound", marker).WithGroup("nested").With("password", "UNREGISTERED_PASSWORD")
				l.Debug("debug " + marker)
				l.Info("info " + marker)
				l.Warn("warning "+marker, "err", fmt.Errorf("wrapped %s", marker))
				l.Error("error "+marker, "config", struct{ Value string }{"UNREGISTERED_CONFIG"})
				log.Printf("standard %q", marker)
				logrus.Warn("native " + marker)
				SDKLogger{}.Logf(smithylog.Warn, "SDK %s", marker)
				text := out.String()
				for _, bad := range []string{marker, "SYNTHETIC_secret", "UNREGISTERED_PASSWORD", "UNREGISTERED_CONFIG"} {
					if strings.Contains(text, bad) {
						t.Fatalf("secret escaped: %s", bad)
					}
				}
				if format == "json" {
					rs := records(t, out.Bytes())
					if len(rs) == 0 {
						t.Fatal("no records")
					}
					for _, r := range rs {
						if r["level"] == "DEBUG" && level != "debug" {
							t.Fatal("debug bypassed threshold")
						}
					}
				}
				if level == "error" && strings.Contains(text, "warning") {
					t.Fatal("warning bypassed error threshold")
				}
			})
		}
	}
}
func TestConfigureAtomicAndLateBoundSecrets(t *testing.T) {
	var out bytes.Buffer
	Install(&out)
	l := slog.With("root", "root-value").WithGroup("group").With("bound", "LATE_REGISTERED_MARKER")
	RegisterSecret("LATE_REGISTERED_MARKER")
	if err := Configure("json", "debug"); err != nil {
		t.Fatal(err)
	}
	if err := Configure("INVALID_SECRET_FORMAT", "info"); err == nil || strings.Contains(err.Error(), "INVALID_SECRET_FORMAT") {
		t.Fatal("invalid format echoed/accepted")
	}
	if err := Configure("text", "INVALID_SECRET_LEVEL"); err == nil || strings.Contains(err.Error(), "INVALID_SECRET_LEVEL") {
		t.Fatal("invalid level echoed/accepted")
	}
	l.Debug("message", "live", true)
	r := records(t, out.Bytes())[0]
	g := r["group"].(map[string]any)
	if g["bound"] != "[REDACTED]" || g["live"] != true || r["root"] != "root-value" {
		t.Fatalf("lost grouped attrs: %#v", r)
	}
}
func TestConcurrentFraming(t *testing.T) {
	var out bytes.Buffer
	Install(&out)
	_ = Configure("json", "debug")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				slog.Info("one\ntwo", "n", j)
			}
		}()
	}
	wg.Wait()
	if n := len(records(t, out.Bytes())); n != 800 {
		t.Fatalf("frames=%d", n)
	}
}
func TestSecretOverlapAndEscaping(t *testing.T) {
	RegisterSecret("OVERLAP", "OVERLAP_LONG", "URL_SECRET /+")
	got := Redact("OVERLAP_LONG OVERLAP URL_SECRET+%2F%2B URL_SECRET%20%2F+")
	if strings.Contains(got, "OVERLAP") || strings.Contains(got, "URL_SECRET") || strings.Contains(got, "_LONG") {
		t.Fatalf("bad redaction %q", got)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("OUTPUT_FAILURE_SECRET") }

func TestNativeTermination(t *testing.T) {
	mode := os.Getenv("S3_SMB_LOG_TEST_CHILD")
	if mode != "" {
		Install(os.Stderr)
		_ = Configure("json", "debug")
		RegisterSecret("TERMINATION_SECRET_MARKER")
		switch mode {
		case "fatal-broken":
			Install(brokenWriter{})
			logrus.Fatal("fatal TERMINATION_SECRET_MARKER")
		case "fatal":
			logrus.Fatal("fatal TERMINATION_SECRET_MARKER")
		case "panic":
			logrus.WithField("password", "UNREGISTERED_PANIC_SECRET").Panic("panic TERMINATION_SECRET_MARKER")
		case "normal":
			logrus.Info("normal")
			SDKLogger{}.Logf(smithylog.Warn, "sdk warning")
			return
		}
		t.Fatal("termination returned")
	}
	for _, mode := range []string{"normal", "fatal", "fatal-broken", "panic"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeTermination$")
			cmd.Env = append(os.Environ(), "S3_SMB_LOG_TEST_CHILD="+mode)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if mode == "normal" && err != nil {
				t.Fatal(err)
			}
			if mode != "normal" && err == nil {
				t.Fatal("expected nonzero exit")
			}
			if strings.HasPrefix(mode, "fatal") {
				if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 1 {
					t.Fatalf("fatal status %v", err)
				}
			}
			for _, stream := range []string{stdout.String(), stderr.String()} {
				for _, marker := range []string{"TERMINATION_SECRET_MARKER", "UNREGISTERED_PANIC_SECRET", "OUTPUT_FAILURE_SECRET"} {
					if strings.Contains(stream, marker) {
						t.Fatalf("%s leaked in %s", marker, mode)
					}
				}
			}
			if mode == "fatal-broken" {
				if stdout.Len()+stderr.Len() != 0 {
					t.Fatal("broken output fell back to another stream")
				}
			} else if mode != "panic" {
				if n := len(records(t, stderr.Bytes())); n == 0 {
					t.Fatal("missing diagnostic")
				}
			} else {
				first := strings.SplitN(stderr.String(), "\n", 2)[0]
				records(t, []byte(first))
			}
		})
	}
}
