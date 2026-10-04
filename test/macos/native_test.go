//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type process struct {
	cmd      *exec.Cmd
	done     chan struct{}
	err      error
	log      *os.File
	terminal *os.File
	drained  chan error
	point    string
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (h *harness) start(name string, cmd *exec.Cmd) *process {
	h.t.Helper()
	log, err := os.OpenFile(filepath.Join(h.evidence, name+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // The log path is built from a harness label and the run-owned evidence directory.
	h.must(err)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		h.must(errors.Join(err, log.Close()))
	}
	p := &process{cmd: cmd, done: make(chan struct{}), log: log}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return p
}

func (h *harness) stop(p *process, abrupt bool, timeout time.Duration) error {
	if p == nil {
		return nil
	}
	wasExited := p.exited()
	var signalErr error
	if !wasExited {
		signal := os.Signal(syscall.SIGTERM)
		if abrupt {
			signal = syscall.SIGKILL
		}
		signalErr = p.cmd.Process.Signal(signal)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	forced := false
	select {
	case <-p.done:
	case <-timer.C:
		forced = true
		signalErr = errors.Join(signalErr, p.cmd.Process.Kill())
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return errors.Join(signalErr, errors.New("process did not exit after kill"))
		}
	}
	var drainErr, closeErr error
	if p.terminal != nil {
		drainErr = p.terminal.Close()
		select {
		case err := <-p.drained:
			drainErr = errors.Join(drainErr, err)
		case <-time.After(5 * time.Second):
			drainErr = errors.Join(drainErr, errors.New("PTY reader did not close"))
		}
		p.terminal = nil
	}
	if p.log != nil {
		closeErr = p.log.Close()
		p.log = nil
	}
	expected := p.err == nil
	if abrupt {
		status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
		expected = ok && status.Signal() == syscall.SIGKILL
	}
	if forced || !expected || wasExited {
		return errors.Join(signalErr, drainErr, closeErr, fmt.Errorf("%s shutdown failed: exited=%t forced=%t status=%s", p.cmd.Path, wasExited, forced, p.cmd.ProcessState))
	}
	return errors.Join(signalErr, drainErr, closeErr)
}

func (h *harness) command(ctx context.Context, timeout time.Duration, directory string, args ...string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	h.serial++
	name := fmt.Sprintf("%04d-%s.log", h.serial, filepath.Base(args[0]))
	h.t.Logf("native-command-start %s %v", name, args)
	cmd := nativeCommand(ctx, args...)
	cmd.Dir = directory
	path := filepath.Join(h.evidence, name)
	log, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // Evidence paths belong to this test run.
	if err != nil {
		return "", err
	}
	cmd.Stdout, cmd.Stderr = log, log
	start := time.Now()
	err = errors.Join(cmd.Run(), log.Close())
	h.save("commands.jsonl", map[string]any{"argv": args, "start": start, "end": time.Now(), "error": fmt.Sprint(err), "output": name}, true)
	h.t.Logf("native-command-exit %s %v", name, err)
	if err != nil {
		return "", fmt.Errorf("%v: %w; see %s", args, err, name)
	}
	if args[0] == "/usr/bin/log" {
		return "", nil
	} // Keep large unified logs on disk.
	output, err := os.ReadFile(path) //nolint:gosec // Read only the command log just created above.
	return string(output), err
}

func (h *harness) run(timeout time.Duration, args ...string) string {
	h.t.Helper()
	output, err := h.command(h.ctx, timeout, "", args...)
	h.must(err)
	return output
}

func (h *harness) native(args ...string) string { h.t.Helper(); return h.run(2*time.Minute, args...) }

func (h *harness) pause(delay time.Duration) {
	h.t.Helper()
	select {
	case <-h.ctx.Done():
		h.t.Fatal(h.ctx.Err())
	case <-time.After(delay):
	}
	if h.daemon != nil && h.daemon.exited() {
		h.t.Fatalf("application exited unexpectedly: %v", h.daemon.err)
	}
}

func nativeCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // Callers supply only harness-owned binaries and native test commands, never shell text.
}

func drainTerminal(terminal *os.File, log io.Writer, phase string, ready chan<- string) error {
	startup := helpers.NewStartup(phase)
	data := make([]byte, 64*1024)
	for {
		count, readErr := terminal.Read(data)
		if _, err := log.Write(data[:count]); err != nil {
			return err
		}
		answer, serving, point, err := startup.Observe(data[:count])
		if err != nil {
			return err
		}
		if answer {
			if _, err := terminal.Write([]byte("yes\n")); err != nil {
				return err
			}
		}
		if serving {
			ready <- point
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, syscall.EIO) || errors.Is(readErr, os.ErrClosed) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (h *harness) startDaemon(phase string) {
	if phase != "restart" {
		h.must(os.Mkdir(h.local, 0o700))
		config := fmt.Sprintf(`smb:
  listen: 127.0.0.1:1445
  share: TimeMachine
  username: timemachine
  password: synthetic-tm-control
storage:
  state_dir: %q
  cache_dir: %q
  cache_size: 0
s3:
  endpoint: http://127.0.0.1:19000
  bucket: time-machine
  region: us-east-1
  path_style: true
  access_key:
    value: mac-acceptance
  secret_key:
    value: synthetic-mac-acceptance-secret
encryption:
  enabled: true
  passphrase:
    value: synthetic-mac-acceptance-passphrase
backup:
  interval: %s
logging:
  format: json
  level: info
`, filepath.Join(h.local, "state"), filepath.Join(h.local, "cache"), h.interval)
		h.must(os.WriteFile(filepath.Join(h.local, "config.yaml"), []byte(config), 0o600))
	}
	h.applicationSerial++
	log, err := os.OpenFile(filepath.Join(h.evidence, fmt.Sprintf("application-%d-%s.log", h.applicationSerial, phase)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	h.must(err)
	cmd := nativeCommand(h.ctx, filepath.Join(h.bin, "s3-smb"), "-c", filepath.Join(h.local, "config.yaml"), "serve")
	terminal, err := pty.Start(cmd)
	if err != nil {
		h.must(errors.Join(err, log.Close()))
	}
	p := &process{cmd: cmd, done: make(chan struct{}), log: log, terminal: terminal, drained: make(chan error, 1)}
	h.daemon = p
	ready := make(chan string, 1)
	go func() { p.drained <- drainTerminal(terminal, log, phase, ready) }()
	go func() { p.err = cmd.Wait(); close(p.done) }()
	select {
	case p.point = <-ready:
	case err := <-p.drained:
		h.t.Fatalf("application evidence: %v", err)
	case <-p.done:
		h.t.Fatalf("application startup: %v", p.err)
	case <-h.ctx.Done():
		h.t.Fatal(h.ctx.Err())
	case <-time.After(3 * time.Minute):
		h.t.Fatal("application startup timeout")
	}
	h.event("application-ready", map[string]any{"phase": phase, "pid": cmd.Process.Pid, "recovered_from": p.point})
}
