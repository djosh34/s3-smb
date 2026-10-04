// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/logging"
)

// SecretSource selects exactly one source. Value and File are pointers, so an
// empty string still counts as set. Command is an argv list and runs without a
// shell.
type SecretSource struct {
	Value   *string  `yaml:"value"`
	File    *string  `yaml:"file"`
	Command []string `yaml:"command"`
}

func (SecretSource) String() string { return "[redacted source]" }

// LogValue keeps the source out of logs.
func (SecretSource) LogValue() slog.Value { return slog.StringValue("[redacted source]") }

func (s SecretSource) validate(label string) error {
	n := 0
	if s.Value != nil {
		n++
	}
	if s.File != nil {
		n++
	}
	if s.Command != nil {
		n++
	}
	if n != 1 {
		return errors.New(label + " requires exactly one of value, file or command")
	}
	if s.File != nil && (*s.File == "" || strings.ContainsRune(*s.File, 0)) {
		return errors.New(label + " file must be a nonempty path without NUL")
	}
	if s.Command != nil {
		if len(s.Command) == 0 || s.Command[0] == "" {
			return errors.New(label + " command requires a nonempty executable")
		}
		for _, arg := range s.Command {
			if strings.ContainsRune(arg, 0) {
				return errors.New(label + " command contains NUL")
			}
		}
	}
	return nil
}

const (
	// HelperTimeout bounds a credential helper command.
	HelperTimeout = 10 * time.Second
	// MaxSecretBytes bounds a secret and a helper's output.
	MaxSecretBytes = 65536
)

func (s SecretSource) resolve(ctx context.Context, dir, label string, logger *slog.Logger) (string, error) {
	var data []byte
	switch {
	case s.Value != nil:
		data = []byte(*s.Value)
	case s.File != nil:
		var err error
		data, err = readFile(*s.File, label, MaxSecretBytes, true, logger)
		if err != nil {
			return "", err
		}
	case s.Command != nil:
		var err error
		data, err = runHelper(ctx, dir, s.Command)
		if err != nil {
			return "", errors.New(label + ": " + err.Error())
		}
	}
	if len(data) > MaxSecretBytes {
		return "", errors.New(label + " exceeds 65536 bytes")
	}
	// Strip one LF or one CRLF only. Preserve all other whitespace and newlines.
	value := strings.TrimSuffix(string(data), "\n")
	if len(value) != len(data) {
		value = strings.TrimSuffix(value, "\r")
	}
	if value == "" || strings.ContainsRune(value, 0) {
		return "", errors.New(label + " must be nonempty and contain no NUL")
	}
	logging.RegisterSecret(value)
	return value, nil
}

type boundedOutput struct {
	cancel    context.CancelFunc
	data      []byte
	remaining int
	keep      bool
	exceeded  bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		b.exceeded = true
		b.cancel()
		return 0, errors.New("helper output limit")
	}
	b.remaining -= len(p)
	if b.keep {
		b.data = append(b.data, p...)
	}
	return len(p), nil
}

func runHelper(parent context.Context, dir string, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, HelperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the helper's process group, so its children and their pipes go too.
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 200 * time.Millisecond
	out := &boundedOutput{remaining: MaxSecretBytes, keep: true, cancel: cancel}
	stderr := &boundedOutput{remaining: MaxSecretBytes, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err := cmd.Run()
	// A helper can exit while a child still holds its pipes. Kill the group
	// again. WaitDelay limits how long Run waited for those pipes.
	if cmd.Process != nil {
		if e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); e != nil && !errors.Is(e, syscall.ESRCH) {
			return nil, errors.New("credential helper processes could not be stopped")
		}
	}
	if out.exceeded || stderr.exceeded {
		return nil, errors.New("credential helper output exceeds 65536 bytes")
	}
	if ctx.Err() != nil {
		return nil, errors.New("credential helper canceled or timed out")
	}
	if err != nil {
		return nil, errors.New("credential helper failed")
	}
	return out.data, nil
}

func readFile(path, label string, limit int64, secret bool, logger *slog.Logger) ([]byte, error) {
	// Nonblocking open lets us reject FIFOs without waiting for a writer.
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New(label + " file could not be opened")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(errors.New(label+" file must be a readable regular file"), f.Close())
	}
	if secret {
		warnPermissions(info, label, logger)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, errors.New(label + " file could not be read")
	}
	if int64(len(data)) > limit {
		return nil, errors.New(label + " file exceeds size limit")
	}
	return data, nil
}

func warnPermissions(info os.FileInfo, label string, logger *slog.Logger) {
	allowed := info.Mode().Perm() == 0o400 || info.Mode().Perm() == 0o600
	if info.IsDir() {
		allowed = info.Mode().Perm() == 0o700
	}
	if !allowed {
		logger.Warn("existing file permissions are not private; continuing without chmod", "setting", label, "mode", info.Mode().Perm().String())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		logger.Warn("existing file has a different owner; continuing without chown", "setting", label)
	}
}

func warnExisting(path, label string, logger *slog.Logger) {
	if path == "" {
		return
	}
	if info, err := os.Stat(path); err == nil {
		warnPermissions(info, label, logger)
	}
}

func (c *Config) secretDir() string {
	if c.path == "" {
		return "."
	}
	return filepath.Dir(c.path)
}
