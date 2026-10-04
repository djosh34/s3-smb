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
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type process struct {
	cmd      *exec.Cmd
	done     chan struct{}
	err      error
	cancel   context.CancelFunc
	log      *os.File
	closeTTY func() error
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
func (p *process) wait() { go func() { p.err = p.cmd.Wait(); close(p.done) }() }

func nativeCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // Callers supply fixed native commands and run-owned binaries, not shell text.
}

func (h *harness) newProcess(name string, args ...string) *process {
	// The phase deadline must leave services alive until cleanup detaches their clients.
	ctx, cancel := context.WithCancel(context.Background())
	log, err := os.OpenFile(filepath.Join(h.evidence, name+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // Logs are created exclusively under this run's evidence directory.
	if err != nil {
		cancel()
		h.must(err)
	}
	cmd := nativeCommand(ctx, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 45 * time.Second
	return &process{cmd: cmd, cancel: cancel, done: make(chan struct{}), log: log}
}

func (h *harness) start(name string, args ...string) *process {
	p := h.newProcess(name, args...)
	if err := p.cmd.Start(); err != nil {
		p.cancel()
		h.must(errors.Join(err, p.log.Close()))
	}
	p.wait()
	return p
}

func stop(p *process, abrupt bool) error {
	defer p.cancel()
	early := p.exited()
	var err error
	if abrupt {
		err = p.cmd.Process.Kill()
	} else {
		p.cancel()
	}
	select {
	case <-p.done:
	case <-time.After(50 * time.Second):
		err = errors.Join(err, errors.New("process exceeded shutdown deadline"), p.cmd.Process.Kill())
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return errors.Join(err, errors.New("process was not reaped"))
		}
	}
	switch {
	case early || p.cmd.ProcessState == nil:
		err = errors.Join(err, errors.New("application or service exited before shutdown"), p.err)
	case abrupt:
		status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || status.Signal() != syscall.SIGKILL {
			err = errors.Join(err, errors.New("application did not exit from SIGKILL"))
		}
	case p.cmd.ProcessState.ExitCode() != 0:
		err = errors.Join(err, errors.New("unsuccessful service shutdown"), p.err)
	case p.err != nil && !errors.Is(p.err, context.Canceled):
		err = errors.Join(err, p.err)
	}
	if p.closeTTY != nil {
		err = errors.Join(err, p.closeTTY())
	}
	return errors.Join(err, p.log.Close())
}

func (h *harness) try(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(h.ctx, timeout)
	defer cancel()
	h.serial++
	name := fmt.Sprintf("%04d-%s.log", h.serial, filepath.Base(args[0]))
	h.t.Logf("native-command-start %s %s %v", time.Now().UTC().Format(time.RFC3339), name, args)
	path := filepath.Join(h.evidence, name)
	log, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // The path is a numbered command log in the run-owned evidence directory.
	if err != nil {
		return "", err
	}
	cmd := nativeCommand(ctx, args...)
	cmd.Stdout, cmd.Stderr = log, log
	err = errors.Join(cmd.Run(), log.Close())
	h.t.Logf("native-command-exit %s %s %v", time.Now().UTC().Format(time.RFC3339), name, err)
	if err != nil {
		return "", fmt.Errorf("%v: %w; see %s", args, err, name)
	}
	if args[0] == "/usr/bin/log" {
		return "", nil
	}
	output, err := os.ReadFile(path) //nolint:gosec // Read only the command log just created above.
	return string(output), err
}

func (h *harness) run(timeout time.Duration, args ...string) string {
	h.t.Helper()
	output, err := h.try(timeout, args...)
	h.must(err)
	return output
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
	name := fmt.Sprintf("application-%d-%s", h.applicationSerial, phase)
	p := h.newProcess(name, filepath.Join(h.bin, "s3-smb"), "-c", filepath.Join(h.local, "config.yaml"), "serve")
	terminal, err := pty.Start(p.cmd)
	if err != nil {
		p.cancel()
		h.must(errors.Join(err, p.log.Close()))
	}
	h.daemon = p
	p.wait()
	ttyPath := filepath.Join(h.evidence, name+"-tty.log")
	ttyLog, err := os.OpenFile(ttyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // The small consent log belongs to this run's application start.
	if err != nil {
		h.must(errors.Join(err, terminal.Close()))
	}
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(ttyLog, terminal)
		if errors.Is(err, syscall.EIO) {
			err = nil
		}
		copied <- err
	}()
	p.closeTTY = func() error {
		var err error
		select {
		case err = <-copied:
		case <-time.After(5 * time.Second):
			err = errors.New("PTY evidence reader did not close")
		}
		return errors.Join(err, terminal.Close(), ttyLog.Close())
	}
	confirmed := false
	h.must(h.waitFor("application startup", 3*time.Minute, 100*time.Millisecond, func() (bool, error) {
		text, err := os.ReadFile(ttyPath) //nolint:gosec // This is the consent log created above.
		if err != nil {
			return false, err
		}
		if !confirmed && strings.Contains(string(text), "Continue? [yes/no]: ") {
			p.point, err = helpers.Confirmation(phase, string(text))
			if err != nil {
				return false, err
			}
			if _, err = terminal.Write([]byte("yes\n")); err != nil {
				return false, err
			}
			confirmed = true
		}
		data, err := os.ReadFile(p.log.Name())
		if err != nil {
			return false, err
		}
		ready := strings.Contains(string(data), `"msg":"SMB serving"`)
		if ready && phase != "restart" && !confirmed {
			return false, errors.New("fresh start did not require documented confirmation")
		}
		return ready, nil
	}))
	h.t.Log("application-ready", phase, p.cmd.Process.Pid, "recovered_from", p.point)
}
