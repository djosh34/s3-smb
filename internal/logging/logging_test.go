// SPDX-License-Identifier: AGPL-3.0-only
package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
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
// attributes and the standard log package.
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
		text := out.String()
		for _, bad := range []string{"SYNTHETIC_secret", "UNREGISTERED_PASSWORD", "UNREGISTERED_CONFIG", "below the level"} {
			if strings.Contains(text, bad) {
				t.Fatalf("%s: %q escaped:\n%s", format, bad, text)
			}
		}
		if lines := strings.Count(text, "\n"); lines != 4 {
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
