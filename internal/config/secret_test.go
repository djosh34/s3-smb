// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/logging"
)

// shell returns a helper command that runs script with sh. Arguments are
// available as $1 and so on.
func shell(script string, args ...string) []string {
	return append([]string{"/bin/sh", "-c", script, "helper"}, args...)
}

func printing(value string) []string { return shell(`printf '%s' "$1"`, value) }

func TestCredentialSources(t *testing.T) {
	file := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(file, []byte("file-private-marker\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		want   string
		source SecretSource
	}{
		{"value-private-marker", SecretSource{Value: ptr("value-private-marker")}},
		{"file-private-marker", SecretSource{File: &file}},
		{"command-private-marker", SecretSource{Command: printing("command-private-marker\n")}},
	} {
		c := mustConfig(t)
		c.S3.AccessKey = test.source
		c.S3.SessionToken = "token-private-marker"
		r, err := c.Resolve(t.Context(), quietLogger())
		if err != nil {
			t.Fatal(err)
		}
		if r.AccessKey != test.want || r.SecretKey != "secret-marker" || r.SessionToken != "token-private-marker" {
			t.Fatalf("resolved %q, want %q", r.AccessKey, test.want)
		}
	}
}

func TestSecretValueRules(t *testing.T) {
	for _, test := range []struct {
		input, want string
		bad         bool
	}{{"", "", true}, {"\n", "", true}, {"\r\n", "", true}, {"a\x00b", "", true}, {" a \n", " a ", false}, {"a\r\n", "a", false}, {"a\n\n", "a\n", false}, {"a\r", "a\r", false}, {strings.Repeat("a", MaxSecretBytes+1), "", true}} {
		v, err := (SecretSource{Value: ptr(test.input)}).resolve(t.Context(), ".", "test", quietLogger())
		if (err != nil) != test.bad || (!test.bad && v != test.want) {
			t.Fatalf("%q: got %q, error %v", test.input, v, err)
		}
	}
}

func TestResolveRegistersSecretsAtOnce(t *testing.T) {
	c := mustConfig(t)
	c.SMB.Password = "smb-distinct-sensitive-marker"
	c.S3.AccessKey = SecretSource{Value: ptr("access-distinct-sensitive-marker")}
	c.S3.SessionToken = "token-distinct-sensitive-marker"
	c.Encryption.Enabled = true
	c.Encryption.Passphrase = SecretSource{Command: printing("phrase-distinct-sensitive-marker")}
	r, err := c.Resolve(t.Context(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.SMB.Password, r.AccessKey, r.SecretKey, r.SessionToken, r.Passphrase} {
		if logging.Redact(secret) != "[REDACTED]" {
			t.Fatal("resolved secret not registered")
		}
	}
	// A later failure must not postpone the registration of earlier secrets.
	c.S3.AccessKey = SecretSource{Value: ptr("first-success-before-failure-marker")}
	c.S3.SecretKey = SecretSource{Command: shell("exit 1")}
	if _, err := c.Resolve(t.Context(), quietLogger()); err == nil {
		t.Fatal("helper failure accepted")
	}
	if logging.Redact(*c.S3.AccessKey.Value) != "[REDACTED]" {
		t.Fatal("first credential registration postponed past later failure")
	}
}

func TestDisabledEncryptionResolvesNoPassphrase(t *testing.T) {
	c := mustConfig(t)
	c.Encryption.Passphrase = SecretSource{Command: []string{"/does-not-exist-private-marker"}, File: ptr("/also-not-present")}
	var logs bytes.Buffer
	r, err := c.Resolve(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil || r.Passphrase != "" {
		t.Fatal("disabled encryption resolved passphrase", err)
	}
	if !strings.Contains(logs.String(), "encryption is disabled") {
		t.Fatalf("missing warning: %s", logs.String())
	}
}

func TestCredentialHelper(t *testing.T) {
	dir := t.TempDir()
	for _, test := range []struct {
		name, want string
		command    []string
	}{
		{"no implicit shell", "$(touch do-not-create) ; $HOME | cat", printing("$(touch do-not-create) ; $HOME | cat")},
		{"no interactive input", "closed-input", shell(`if [ -n "$(cat)" ]; then exit 3; fi; printf closed-input`)},
		{"working directory", dir, shell("pwd")},
	} {
		v, err := (SecretSource{Command: test.command}).resolve(t.Context(), dir, "credential", quietLogger())
		if err != nil || v != test.want {
			t.Fatalf("%s: %q, %v", test.name, v, err)
		}
	}
}

func TestCredentialHelperFailuresAreBoundedAndPrivate(t *testing.T) {
	limit := strconv.Itoa(MaxSecretBytes + 1)
	for name, command := range map[string][]string{
		"exit status":        shell(`echo stderr-private-marker >&2; printf stdout-private-marker; exit 2`, "argv-private-marker"),
		"stdout limit":       shell(`head -c "$1" /dev/zero`, limit),
		"stderr limit":       shell(`head -c "$1" /dev/zero >&2`, limit),
		"missing executable": {"/missing-private-marker"},
	} {
		_, err := (SecretSource{Command: command}).resolve(t.Context(), ".", "credential", quietLogger())
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := (SecretSource{Command: shell("sleep 600")}).resolve(ctx, ".", "credential", quietLogger())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("canceled helper: %v", err)
	}
}

func TestSecretFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret-private-marker")
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Missing files, directories and FIFOs fail without waiting for a writer.
	for _, name := range []string{path, dir, fifo} {
		_, err := (SecretSource{File: &name}).resolve(t.Context(), ".", "credential", quietLogger())
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxSecretBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (SecretSource{File: &path}).resolve(t.Context(), ".", "credential", quietLogger()); err == nil {
		t.Fatal("oversize file accepted")
	}
}

func TestSecretFilePermissionsWarnOnly(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	path := filepath.Join(t.TempDir(), "path-private-marker")
	if err := os.WriteFile(path, []byte("secret-private-marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o440, 0o600, 0o400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		logs.Reset()
		value, err := (SecretSource{File: &path}).resolve(t.Context(), ".", "s3.access_key", logger)
		if err != nil || value != "secret-private-marker" {
			t.Fatalf("mode %v: %v", mode, err)
		}
		warned := strings.Contains(logs.String(), "continuing without chmod")
		if warned != (mode == 0o440) || strings.Contains(logs.String(), "private-marker") {
			t.Fatalf("mode %v: %s", mode, logs.String())
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("changed mode", err)
		}
	}
}

type foreignOwnerInfo struct{ os.FileInfo }

func (foreignOwnerInfo) Sys() any { return &syscall.Stat_t{Uid: ^uint32(0)} }

func TestDifferentOwnerWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-marker")
	if err := os.WriteFile(path, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	warnPermissions(foreignOwnerInfo{info}, "credential", slog.New(slog.NewJSONHandler(&b, nil)))
	if !strings.Contains(b.String(), "different owner") || strings.Contains(b.String(), "private-marker") {
		t.Fatal(b.String())
	}
}

func TestConfigurationPrintsRedacted(t *testing.T) {
	c := mustConfig(t)
	r, err := c.Resolve(t.Context(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{c, r, c.S3.AccessKey} {
		var b bytes.Buffer
		slog.New(slog.NewJSONHandler(&b, nil)).Info("test redaction", "value", value)
		if strings.Contains(fmt.Sprint(value), "marker") || strings.Contains(b.String(), "marker") || !json.Valid(b.Bytes()) {
			t.Fatalf("%T leaks: %s", value, b.String())
		}
	}
}
