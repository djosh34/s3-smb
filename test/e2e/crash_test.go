// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	smb "github.com/hirochachacha/go-smb2"
)

func crashBaseline() map[string][]byte {
	data := make([]byte, 1_000_003)
	for i := range data {
		data[i] = byte((i*19 + i/131) % 251)
	}
	return map[string][]byte{"baseline-empty": {}, "baseline-文件.txt": []byte("completed before observed in-flight S3 request\n"), "baseline-large.bin": data}
}
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
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		done <- err
	}()
	return done
}
func waitHeldPut(t *testing.T, held <-chan faultEvent, write <-chan error) faultEvent {
	t.Helper()
	select {
	case event := <-held:
		select {
		case err := <-write:
			t.Fatalf("SMB write/flush already returned before interruption: %v", err)
		default:
		}
		if event.Method != "PUT" || !strings.Contains(event.Path, "/chunks/") || event.Status < 200 || event.Status >= 300 {
			t.Fatalf("not a held actual successful chunk PUT: %+v", event)
		}
		return event
	case err := <-write:
		t.Fatalf("SMB write ended without observed held chunk PUT: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no real chunk PUT observed while SMB write/flush pending")
	}
	return faultEvent{}
}
func killAtObservedPut(t *testing.T, d *daemon, p *faultProxy) {
	t.Helper()
	sigkill(t, d)
	p.Release()
}
func sigkill(t *testing.T, d *daemon) {
	t.Helper()
	if err := d.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-d.done:
		d.stopped = true
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("SIGKILL did not terminate daemon nonzero: %v", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("daemon did not exit from intentional SIGKILL: %v", err)
		}
		d.closeLogs()
	case <-time.After(5 * time.Second):
		t.Fatal("SIGKILL daemon exit deadline")
	}
}
func waitInterruptedSMB(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted SMB write/flush falsely reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SMB client did not observe interrupted daemon")
	}
}

func TestCrashDuringChunkPut(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, encrypted)
			p := newFaultProxy(t, f.endpoint)
			f.endpoint = p.URL()
			d := f.start()
			s, closeShare := f.share()
			files := crashBaseline()
			for name, data := range files {
				writeFile(t, s, name, data)
			}
			verifyFiles(t, s, files)
			closeShare()
			f.protectedAfter(time.Now())
			s, closeShare = f.share()
			held := p.HoldNextChunkResponse()
			pending := pendingSMBWrite(s, "interrupted-new-file.bin", bytes.Repeat([]byte("subsequent-SMB-write\n"), 500_000))
			waitHeldPut(t, held, pending)
			killAtObservedPut(t, d, p)
			waitInterruptedSMB(t, pending)
			closeShare()
			// Preserve the actual MinIO bucket, discard every daemon-local path and
			// confirm normal startup's selected latest native recovery point via PTY.
			f.freshLocal()
			d = f.start()
			s, closeShare = f.share()
			verifyFiles(t, s, files)
			files["resumed-after-crash.txt"] = []byte("successful resumed SMB write after cold S3 recovery\n")
			writeFile(t, s, "resumed-after-crash.txt", files["resumed-after-crash.txt"])
			closeShare()
			f.protectedAfter(time.Now())
			d.stop()
			f.freshLocal()
			d = f.start()
			s, closeShare = f.share()
			verifyFiles(t, s, files)
			closeShare()
			d.stop()
		})
	}
}

func TestStalledIOBoundedShutdown(t *testing.T) {
	f := newFixture(t, true)
	p := newFaultProxy(t, f.endpoint)
	f.endpoint = p.URL()
	d := f.start()
	s, closeShare := f.share()
	defer closeShare()
	held := p.HoldNextChunkResponse()
	pending := pendingSMBWrite(s, "held-native-upload.bin", bytes.Repeat([]byte("stalled-native-upload\n"), 500_000))
	waitHeldPut(t, held, pending)
	lock, err := os.OpenFile(filepath.Join(f.root, "state", "state.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	locked := func() bool {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			return false
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("inspect state lock: %v", err)
		}
		return true
	}
	if !locked() {
		t.Fatal("daemon did not hold local authority lock before shutdown")
	}
	start := time.Now()
	if err = d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(38 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var exitErr error
wait:
	for {
		select {
		case exitErr = <-d.done:
			break wait
		case <-tick.C:
			if !locked() {
				// OS unlock and cmd.Wait delivery are not atomic. Permit only termination
				// delivery latency, not a live process that released authority early.
				select {
				case exitErr = <-d.done:
					break wait
				case <-time.After(250 * time.Millisecond):
					t.Fatal("state lock released while stalled daemon remained alive")
				}
			}
		case <-deadline.C:
			t.Fatal("stalled native I/O exceeded bounded shutdown deadline")
		}
	}
	d.stopped = true
	p.Release()
	d.closeLogs()
	if exitErr == nil {
		t.Fatal("unflushed stalled native I/O returned successful process exit")
	}
	if time.Since(start) > 35*time.Second {
		t.Fatalf("shutdown exceeded 30s watchdog plus scheduling tolerance: %s", time.Since(start))
	}
	if locked() {
		t.Fatal("OS did not release state lock after actual process termination")
	}
	var output []byte
	for _, file := range d.logs {
		data, e := os.ReadFile(file.Name())
		if e != nil {
			t.Fatal(e)
		}
		output = append(output, data...)
	}
	// The SMB shutdown context and the hard-exit timer both expire after 30
	// seconds, and either one can report first.
	if !bytes.Contains(output, []byte("shutdown deadline exceeded")) && !bytes.Contains(output, []byte("SMB shutdown failed; state lock retained: context deadline exceeded")) {
		t.Fatal("stalled native operation did not report explicit shutdown deadline failure")
	}
	waitInterruptedSMB(t, pending)
	t.Logf("actual pending native S3 I/O: nonzero process exit after %s; authority lock unavailable until termination", time.Since(start))
}
