//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type process struct {
	cmd    *exec.Cmd
	done   chan struct{}
	err    error
	cancel context.CancelFunc
	log    *os.File
	// point is the database copy that an application start restored.
	point string
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

// command runs name with args directly, without a shell.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

func (h *harness) newProcess(name string, args ...string) *process {
	// The phase deadline must leave services alive until cleanup detaches their clients.
	ctx, cancel := context.WithCancel(context.Background())
	log, err := h.evidenceDir.OpenFile(name+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cancel()
		h.must(err)
	}
	cmd := command(ctx, args[0], args[1:]...)
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
	return errors.Join(err, p.log.Close())
}

// try runs a native command and saves its output as a numbered evidence log.
// It returns the output, except for /usr/bin/log, whose output can be large.
func (h *harness) try(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(h.ctx, timeout)
	defer cancel()
	h.serial++
	name := fmt.Sprintf("%04d-%s.log", h.serial, filepath.Base(args[0]))
	h.t.Logf("native-command-start %s %s %v", time.Now().UTC().Format(time.RFC3339), name, args)
	log, err := h.evidenceDir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	cmd := command(ctx, args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	err = errors.Join(cmd.Run(), log.Close())
	h.t.Logf("native-command-exit %s %s %v", time.Now().UTC().Format(time.RFC3339), name, err)
	var output []byte
	if args[0] != "/usr/bin/log" {
		var readErr error
		output, readErr = h.evidenceDir.ReadFile(name)
		err = errors.Join(err, readErr)
	}
	if err != nil {
		return string(output), fmt.Errorf("%v: %w; see %s", args, err, name)
	}
	return string(output), nil
}

func (h *harness) run(timeout time.Duration, args ...string) string {
	h.t.Helper()
	output, err := h.try(timeout, args...)
	h.must(err)
	return output
}

// startDaemon starts s3-smb and waits until it serves. A fresh start writes
// the config into a new data folder. A recover start also has a new data
// folder and must restore a database copy. It may first wait out the old
// server's stale bucket lock, which takes 10 minutes. A restart reuses the
// data folder and must keep its database.
func (h *harness) startDaemon(phase string) {
	if phase != "restart" {
		h.must(h.workDir.Mkdir("daemon", 0o700))
		config := fmt.Sprintf(`smb:
  listen: 127.0.0.1:1445
  share: TimeMachine
  username: timemachine
  password: synthetic-tm-control
storage:
  state_dir: %q
s3:
  endpoint: http://127.0.0.1:19000
  bucket: %s
  region: us-east-1
  path_style: true
  access_key:
    value: %s
  secret_key:
    value: %s
logging:
  format: json
  level: info
`, filepath.Join(h.local, "state"), bucket, minioUser, minioPassword)
		h.must(h.workDir.WriteFile("daemon/config.yaml", []byte(config), 0o600))
	}
	h.applicationSerial++
	name := fmt.Sprintf("application-%d-%s", h.applicationSerial, phase)
	p := h.start(name, filepath.Join(h.bin, "s3-smb"), "-c", filepath.Join(h.local, "config.yaml"), "serve")
	h.daemon = p
	limit := 3 * time.Minute
	if phase == "recover" {
		limit = 15 * time.Minute
	}
	h.must(h.waitFor("application startup", limit, time.Second, func() (bool, error) {
		data, err := h.evidenceDir.ReadFile(name + ".log")
		if err != nil {
			return false, err
		}
		text := string(data)
		if !strings.Contains(text, `"msg":"SMB serving"`) {
			return false, nil
		}
		p.point = helpers.RestoredCopy(text)
		kept := strings.Contains(text, `"msg":"keeping the local database"`)
		switch {
		case phase == "recover" && p.point == "":
			return false, errors.New("recovery start restored no database copy")
		case phase == "restart" && !kept:
			return false, errors.New("restart did not keep its local database")
		case phase == "initialize" && (p.point != "" || kept):
			return false, errors.New("fresh start found an existing database")
		}
		return true, nil
	}))
	h.t.Log("application-ready", phase, p.cmd.Process.Pid, "restored", p.point)
}
