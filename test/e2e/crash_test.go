// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	smb "github.com/hirochachacha/go-smb2"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

// crashBaseline returns files written and backed up before a crash.
func crashBaseline() map[string][]byte {
	data := make([]byte, 1_000_003)
	for i := range data {
		data[i] = byte((i*19 + i/131) % 251)
	}
	return map[string][]byte{"baseline-empty": {}, "baseline-文件.txt": []byte("completed before observed in-flight S3 request\n"), "baseline-large.bin": data}
}

// pendingSMBWrite writes and flushes a file in the background.
func pendingSMBWrite(share *smb.Share, name string, data []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		file, err := share.Create(name)
		if err != nil {
			done <- err
			return
		}
		n, err := file.Write(data)
		if err == nil && n != len(data) {
			err = fmt.Errorf("short SMB write %d/%d", n, len(data))
		}
		if err == nil {
			err = file.Sync()
		}
		done <- errors.Join(err, file.Close())
	}()
	return done
}

// waitHeldPut waits until S3 has accepted a chunk of the pending SMB write and
// the proxy holds the response.
func waitHeldPut(t *testing.T, held <-chan s3fault.Event, write <-chan error) {
	t.Helper()
	select {
	case event := <-held:
		select {
		case err := <-write:
			t.Fatalf("SMB write returned before the interruption: %v", err)
		default:
		}
		if event.Method != http.MethodPut || !strings.Contains(event.Path, "/chunks/") {
			t.Fatalf("held request is not a chunk PUT: %+v", event)
		}
	case err := <-write:
		t.Fatalf("SMB write ended before a chunk PUT was held: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no chunk PUT while the SMB write was pending")
	}
}

// sigkill kills the daemon and checks that SIGKILL ended it.
func sigkill(t *testing.T, d *daemon) {
	t.Helper()
	err := d.kill()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("SIGKILL did not end the daemon with an error: %v", err)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("daemon did not exit from SIGKILL: %v", err)
	}
}

func waitInterruptedSMB(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted SMB write reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SMB client did not notice the interrupted daemon")
	}
}

// TestCrashDuringChunkPut kills the daemon while S3 holds the reply to a chunk
// PUT, then recovers from S3 with no local state.
func TestCrashDuringChunkPut(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, encrypted)
			proxy := f.newFaultProxy()
			d := f.start()
			share, disconnect := f.share()
			files := crashBaseline()
			for name, data := range files {
				writeFile(t, share, name, data)
			}
			verifyFiles(t, share, files)
			disconnect()
			f.protectedAfter(time.Now())
			// The kill ends this connection, so it is never logged off.
			share, _ = f.share()
			held, err := proxy.HoldNextChunkResponse()
			if err != nil {
				t.Fatal(err)
			}
			pending := pendingSMBWrite(share, "interrupted-new-file.bin", bytes.Repeat([]byte("subsequent-SMB-write\n"), 500_000))
			waitHeldPut(t, held, pending)
			sigkill(t, d)
			proxy.Release()
			waitInterruptedSMB(t, pending)
			recoverTwice(t, f, files)
		})
	}
}

// TestStalledIOBoundedShutdown stops the daemon while a chunk PUT hangs. The
// daemon must exit with an error within its 30 second shutdown limit and keep
// the state lock until it exits.
func TestStalledIOBoundedShutdown(t *testing.T) {
	f := newFixture(t, true)
	proxy := f.newFaultProxy()
	d := f.start()
	// The shutdown ends this connection, so it is never logged off.
	share, _ := f.share()
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingSMBWrite(share, "held-native-upload.bin", bytes.Repeat([]byte("stalled-native-upload\n"), 500_000))
	waitHeldPut(t, held, pending)
	lock := openStateLock(t, f)
	if !lock.held(t) {
		t.Fatal("daemon did not hold the state lock before shutdown")
	}
	start := time.Now()
	if err = d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exitErr := waitLockedExit(t, d, lock)
	proxy.Release()
	d.exited()
	if exitErr == nil {
		t.Fatal("daemon with an unflushed write exited successfully")
	}
	if time.Since(start) > 35*time.Second {
		t.Fatalf("shutdown took %s, more than the 30 second limit", time.Since(start))
	}
	if lock.held(t) {
		t.Fatal("state lock still held after the daemon exited")
	}
	// The SMB shutdown context and the hard-exit timer both expire after 30
	// seconds, and either one can report first.
	output := d.output()
	if !bytes.Contains(output, []byte("shutdown deadline exceeded")) && !bytes.Contains(output, []byte("SMB shutdown failed; state lock retained: context deadline exceeded")) {
		t.Fatal("daemon did not report the shutdown deadline")
	}
	waitInterruptedSMB(t, pending)
}

type stateLock struct{ file *os.File }

func openStateLock(t *testing.T, f *fixture) stateLock {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(f.root, "state", "state.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, file)
	return stateLock{file: file}
}

// held reports whether another process holds the lock.
func (lock stateLock) held(t *testing.T) bool {
	t.Helper()
	err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if err == nil {
		err = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	}
	if err != nil {
		t.Fatalf("inspect state lock: %v", err)
	}
	return false
}

// waitLockedExit waits for the daemon to exit and fails if it releases the
// state lock while still running.
func waitLockedExit(t *testing.T, d *daemon, lock stateLock) error {
	t.Helper()
	deadline := time.NewTimer(38 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-d.done:
			return err
		case <-tick.C:
			if lock.held(t) {
				continue
			}
			// The kernel drops the lock just before the exit reaches cmd.Wait.
			select {
			case err := <-d.done:
				return err
			case <-time.After(250 * time.Millisecond):
				t.Fatal("state lock released while the daemon kept running")
			}
		case <-deadline.C:
			t.Fatal("daemon did not exit after its shutdown limit")
		}
	}
}
