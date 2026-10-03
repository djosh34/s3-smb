//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only
package macos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
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
	log, err := os.OpenFile(filepath.Join(h.evidence, name+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = directory
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	start := time.Now()
	err := cmd.Run()
	writeErr := os.WriteFile(filepath.Join(h.evidence, name), output.Bytes(), 0o600)
	h.save("commands.jsonl", map[string]any{"argv": args, "start": start, "end": time.Now(), "error": fmt.Sprint(err), "output": name}, true)
	h.t.Logf("native-command-exit %s %v", name, err)
	if err != nil {
		err = fmt.Errorf("%v: %w; see %s", args, err, name)
	}
	return output.String(), errors.Join(err, writeErr)
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

var pointPattern = regexp.MustCompile(`Recover metadata from (\S+)`)

func drainTerminal(terminal *os.File, log io.Writer, phase string, ready chan<- string) error {
	var buffer []byte
	confirmed, reported := false, false
	data := make([]byte, 64*1024)
	for {
		count, err := terminal.Read(data)
		if count > 0 {
			if _, writeErr := log.Write(data[:count]); writeErr != nil {
				return writeErr
			}
			buffer = append(buffer, data[:count]...)
			if len(buffer) > 128*1024 {
				buffer = buffer[len(buffer)-128*1024:]
			}
			if bytes.Contains(buffer, []byte("Continue? [yes/no]: ")) && !confirmed {
				expected := "Initialize a genuinely empty S3 dataset?"
				if phase == "recover" {
					expected = "Recover metadata from "
				}
				if phase == "restart" || !bytes.Contains(buffer, []byte(expected)) {
					return errors.New("unexpected application confirmation; refusing automatic answer")
				}
				if _, err := terminal.Write([]byte("yes\n")); err != nil {
					return err
				}
				confirmed = true
			}
			if bytes.Contains(buffer, []byte(`"msg":"SMB serving"`)) && !reported {
				if phase != "restart" && !confirmed {
					return errors.New("fresh start did not require documented confirmation")
				}
				point := ""
				if match := pointPattern.FindSubmatch(buffer); match != nil {
					point = string(match[1])
				}
				ready <- point
				reported = true
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
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
	cmd := exec.CommandContext(h.ctx, filepath.Join(h.bin, "s3-smb"), "-c", filepath.Join(h.local, "config.yaml"), "serve")
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
	if phase == "recover" && strings.TrimSpace(p.point) == "" {
		h.t.Fatal("recovery did not name a metadata point")
	}
	h.event("application-ready", map[string]any{"phase": phase, "pid": cmd.Process.Pid, "recovered_from": p.point})
}
