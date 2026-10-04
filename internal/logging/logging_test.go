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

func install(t *testing.T, w *bytes.Buffer, format, level string) {
	t.Helper()
	Install(w)
	t.Cleanup(func() { Install(os.Stderr) })
	if err := Configure(format, level); err != nil {
		t.Fatal(err)
	}
}

// Every route into the log is redacted: slog with bound and grouped
// attributes, the standard log package, logrus and the AWS SDK.
func TestEveryLogRouteIsRedacted(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		var out bytes.Buffer
		install(t, &out, format, "info")
		marker := "SYNTHETIC_secret/with space\nnewline_91"
		RegisterSecret(marker)
		l := slog.With("bound", marker).WithGroup("nested").With("password", "UNREGISTERED_PASSWORD")
		l.Debug("debug below the level")
		l.Info("info " + marker)
		l.Warn("warning "+marker, "err", fmt.Errorf("wrapped %s", marker))
		l.Error("error "+marker, "config", struct{ Value string }{"UNREGISTERED_CONFIG"})
		log.Printf("standard %q", marker)
		logrus.Warn("native " + marker)
		SDKLogger{}.Logf(smithylog.Warn, "SDK %s", marker)
		text := out.String()
		for _, bad := range []string{"SYNTHETIC_secret", "UNREGISTERED_PASSWORD", "UNREGISTERED_CONFIG", "below the level"} {
			if strings.Contains(text, bad) {
				t.Fatalf("%s: %q escaped:\n%s", format, bad, text)
			}
		}
		if lines := strings.Count(text, "\n"); lines != 6 {
			t.Fatalf("%s: %d lines:\n%s", format, lines, text)
		}
	}
}

func TestConfigureIsAtomicAndSecretsBindLate(t *testing.T) {
	var out bytes.Buffer
	install(t, &out, "text", "info")
	l := slog.With("root", "root-value").WithGroup("group").With("bound", "LATE_REGISTERED_MARKER")
	RegisterSecret("LATE_REGISTERED_MARKER")
	if err := Configure("json", "debug"); err != nil {
		t.Fatal(err)
	}
	if err := Configure("INVALID_SECRET_FORMAT", "info"); err == nil || strings.Contains(err.Error(), "INVALID_SECRET_FORMAT") {
		t.Fatal("invalid format echoed or accepted")
	}
	if err := Configure("text", "INVALID_SECRET_LEVEL"); err == nil || strings.Contains(err.Error(), "INVALID_SECRET_LEVEL") {
		t.Fatal("invalid level echoed or accepted")
	}
	l.Debug("message", "live", true)
	r := records(t, out.Bytes())[0]
	g, ok := r["group"].(map[string]any)
	if !ok || g["bound"] != "[REDACTED]" || g["live"] != true || r["root"] != "root-value" {
		t.Fatalf("lost grouped attrs: %#v", r)
	}
}

func TestConcurrentFraming(t *testing.T) {
	var out bytes.Buffer
	install(t, &out, "json", "debug")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for j := range 100 {
				slog.Info("one\ntwo", "n", j)
			}
		})
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

// runTermination makes logrus terminate the process in the given way.
func runTermination(mode string) {
	Install(os.Stderr)
	if err := Configure("json", "debug"); err != nil {
		return
	}
	RegisterSecret("TERMINATION_SECRET_MARKER")
	switch mode {
	case "fatal-broken":
		Install(brokenWriter{})
		logrus.Fatal("fatal TERMINATION_SECRET_MARKER")
	case "fatal":
		logrus.Fatal("fatal TERMINATION_SECRET_MARKER")
	case "panic":
		logrus.WithField("password", "UNREGISTERED_PANIC_SECRET").Panic("panic TERMINATION_SECRET_MARKER")
	}
}

// logrus Fatal and Panic end the process. Their output stays redacted and
// in the configured format, and a broken log output is not replaced.
func TestNativeTermination(t *testing.T) {
	if mode := os.Getenv("S3_SMB_LOG_TEST_CHILD"); mode != "" {
		runTermination(mode)
		t.Fatal("termination returned")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"fatal", "fatal-broken", "panic"} {
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestNativeTermination$")
		cmd.Env = append(os.Environ(), "S3_SMB_LOG_TEST_CHILD="+mode)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || mode != "panic" && exit.ExitCode() != 1 {
			t.Fatalf("%s: exit %v", mode, err)
		}
		output := stdout.String() + stderr.String()
		for _, marker := range []string{"TERMINATION_SECRET_MARKER", "UNREGISTERED_PANIC_SECRET", "OUTPUT_FAILURE_SECRET"} {
			if strings.Contains(output, marker) {
				t.Fatalf("%s: %s leaked:\n%s", mode, marker, output)
			}
		}
		first, _, _ := strings.Cut(stderr.String(), "\n")
		switch {
		case mode == "fatal-broken" && output != "":
			t.Fatalf("broken output fell back to another stream:\n%s", output)
		case mode != "fatal-broken" && len(records(t, []byte(first))) != 1:
			t.Fatalf("%s: missing diagnostic:\n%s", mode, output)
		}
	}
}
