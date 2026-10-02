package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func helperArgs(mode string, args ...string) []string {
	argv := []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", "s3-smb-config-helper", mode}
	return append(argv, args...)
}
func TestHelperProcess(t *testing.T) {
	i := 0
	for ; i < len(os.Args); i++ {
		if os.Args[i] == "s3-smb-config-helper" {
			break
		}
	}
	if i == len(os.Args) {
		return
	}
	args := os.Args[i+1:]
	switch args[0] {
	case "echo":
		fmt.Print(args[1])
	case "fail":
		fmt.Fprintln(os.Stderr, "stderr-private-marker")
		fmt.Print("stdout-private-marker")
		os.Exit(2)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "stdout-limit":
		fmt.Print(strings.Repeat("x", MaxSecretBytes+1))
	case "stderr-limit":
		fmt.Fprint(os.Stderr, strings.Repeat("x", MaxSecretBytes+1))
	case "stdin":
		data, _ := io.ReadAll(os.Stdin)
		if len(data) != 0 {
			os.Exit(3)
		}
		fmt.Print("closed-input")
	case "cwd":
		dir, _ := os.Getwd()
		fmt.Print(dir)
	default:
		os.Exit(4)
	}
	os.Exit(0)
}
func TestHelperBoundedPrivateDirectArgv(t *testing.T) {
	for _, mode := range []string{"fail", "stdout-limit", "stderr-limit"} {
		t.Run(mode, func(t *testing.T) {
			s := SecretSource{Command: helperArgs(mode, "argv-private-marker")}
			start := time.Now()
			_, err := s.resolve(context.Background(), ".", "credential", quietLogger())
			if err == nil {
				t.Fatal("expected failure")
			}
			if strings.Contains(err.Error(), "private-marker") || time.Since(start) > 5*time.Second {
				t.Fatal("unsafe/unbounded helper failure", err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := (SecretSource{Command: helperArgs("sleep")}).resolve(ctx, ".", "credential", quietLogger())
		if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 2*time.Second {
			t.Fatal("timeout not bounded", err)
		}
	})
	t.Run("no implicit shell", func(t *testing.T) {
		s := SecretSource{Command: helperArgs("echo", "$(touch do-not-create) ; $HOME | cat")}
		v, err := s.resolve(context.Background(), ".", "credential", quietLogger())
		if err != nil || v != "$(touch do-not-create) ; $HOME | cat" {
			t.Fatal("argv altered", err)
		}
	})
	t.Run("no interactive input", func(t *testing.T) {
		v, err := (SecretSource{Command: helperArgs("stdin")}).resolve(context.Background(), ".", "credential", quietLogger())
		if err != nil || v != "closed-input" {
			t.Fatal(v, err)
		}
	})
	t.Run("working directory", func(t *testing.T) {
		dir := t.TempDir()
		v, err := (SecretSource{Command: helperArgs("cwd")}).resolve(context.Background(), dir, "credential", quietLogger())
		if err != nil || v != dir {
			t.Fatal(v, err)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		_, err := (SecretSource{Command: []string{"/missing-private-marker"}}).resolve(context.Background(), ".", "credential", quietLogger())
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal(err)
		}
	})
}
func TestSecretFilesWarnWithoutChangingPermissions(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var logs bytes.Buffer
			var handler slog.Handler = slog.NewTextHandler(&logs, nil)
			if format == "json" {
				handler = slog.NewJSONHandler(&logs, nil)
			}
			logger := slog.New(handler)
			path := filepath.Join(t.TempDir(), "path-private-marker")
			if err := os.WriteFile(path, []byte("secret-private-marker\n"), 0644); err != nil {
				t.Fatal(err)
			}
			value, err := (SecretSource{File: &path}).resolve(context.Background(), ".", "s3.access_key", logger)
			if err != nil || value != "secret-private-marker" {
				t.Fatal("readable permissive file rejected", err)
			}
			if !strings.Contains(logs.String(), "continuing without chmod") || strings.Contains(logs.String(), "private-marker") {
				t.Fatal("warning missing or leaked", logs.String())
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0644 {
				t.Fatal("modified mode", err)
			}
			if format == "json" {
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					if !json.Valid([]byte(line)) {
						t.Fatal("invalid JSON", line)
					}
				}
			}
			for _, mode := range []os.FileMode{0400, 0600} {
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				logs.Reset()
				if _, err := (SecretSource{File: &path}).resolve(context.Background(), ".", "s3.access_key", logger); err != nil || logs.Len() != 0 {
					t.Fatal("private file warns", err, logs.String())
				}
			}
		})
	}
}
func TestSecretFileRealFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret-private-marker")
	for _, name := range []string{path, dir} {
		_, err := (SecretSource{File: &name}).resolve(context.Background(), ".", "credential", quietLogger())
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxSecretBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (SecretSource{File: &path}).resolve(context.Background(), ".", "credential", quietLogger()); err == nil {
		t.Fatal("oversize file accepted")
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(path, 0000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(path, 0600)
		if _, err := (SecretSource{File: &path}).resolve(context.Background(), ".", "credential", quietLogger()); err == nil {
			t.Fatal("unreadable file accepted")
		}
	} else {
		t.Log("root bypasses Unix mode read denial; unreadable-mode assertion not exercised")
	}
}

type foreignOwnerInfo struct{ os.FileInfo }

func (f foreignOwnerInfo) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Geteuid() + 1)} }
func TestDifferentOwnerWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-marker")
	if err := os.WriteFile(path, []byte("value"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, jsonMode := range []bool{false, true} {
		var b bytes.Buffer
		var h slog.Handler = slog.NewTextHandler(&b, nil)
		if jsonMode {
			h = slog.NewJSONHandler(&b, nil)
		}
		warnPermissions(foreignOwnerInfo{info}, "credential", slog.New(h))
		if !strings.Contains(b.String(), "different owner") || strings.Contains(b.String(), "private-marker") {
			t.Fatal(b.String())
		}
		if jsonMode && !json.Valid(bytes.TrimSpace(b.Bytes())) {
			t.Fatal("invalid warning JSON")
		}
	}
}
func TestDisabledEncryptionWarning(t *testing.T) {
	var b bytes.Buffer
	_, err := mustConfig(t).Resolve(context.Background(), slog.New(slog.NewJSONHandler(&b, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "encryption is disabled") || strings.Contains(b.String(), "secret-marker") {
		t.Fatal("missing warning/leak", b.String())
	}
}
