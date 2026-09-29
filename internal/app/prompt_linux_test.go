// SPDX-License-Identifier: AGPL-3.0-only
//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/logging"
	"golang.org/x/sys/unix"
)

// Linux's actual controlling PTY is the fixture: stdin/stdout/stderr are not
// substituted for /dev/tty, and diagnostics remain a separate JSON stream.
func TestControllingTTYConfirmation(t *testing.T) {
	if os.Getenv("S3_SMB_APP_PTY_TEST") == "1" {
		logging.Install(os.Stderr)
		if err := logging.Configure("json", "info"); err != nil {
			os.Exit(98)
		}
		if err := confirm("PTY dataset confirmation"); err != nil {
			slog.Error("confirmation failed", "error", err)
			os.Exit(1)
		}
		slog.Info("confirmation accepted")
		os.Exit(0)
	}
	for _, tc := range []struct {
		name, input string
		code        int
	}{{"yes", "yes\n", 0}, {"no", "no\n", 1}, {"short-yes", "y\n", 1}, {"eof", "\x04", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestControllingTTYConfirmation$")
			cmd.Env = append(os.Environ(), "S3_SMB_APP_PTY_TEST=1")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			cmd.Stdin = slave
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			prompt, err := readPTYPrompt(master)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, "PTY dataset confirmation") {
				t.Fatalf("wrong terminal output: %q", prompt)
			}
			if _, err = master.WriteString(tc.input); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			if ctx.Err() != nil || cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("exit=%d err=%v stderr=%q", cmd.ProcessState.ExitCode(), err, stderr.Bytes())
			}
			if stdout.Len() != 0 {
				t.Fatalf("prompt contaminated stdout: %q", stdout.Bytes())
			}
			if bytes.Contains(stderr.Bytes(), []byte("Continue?")) {
				t.Fatal("prompt contaminated diagnostics")
			}
			var record map[string]any
			if err = json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &record); err != nil {
				t.Fatalf("diagnostics not one valid JSON record: %q", stderr.Bytes())
			}
		})
	}
}

func readPTYPrompt(master *os.File) (string, error) {
	var output strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(output.String(), "Continue? [yes/no]: ") {
		if time.Now().After(deadline) {
			return output.String(), fmt.Errorf("timed out waiting for tty prompt")
		}
		fds := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return output.String(), err
		}
		if n == 0 {
			continue
		}
		var buf [1024]byte
		n, err = unix.Read(int(master.Fd()), buf[:])
		if err != nil {
			return output.String(), err
		}
		if n == 0 {
			return output.String(), fmt.Errorf("tty closed before prompt")
		}
		output.Write(buf[:n])
		if output.Len() > 4096 {
			return output.String(), fmt.Errorf("unexpected oversized tty output")
		}
	}
	return output.String(), nil
}
