//go:build smbnext

// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/storage"
)

func TestShutdownSignalProcess(t *testing.T) {
	dir := os.Getenv("S3_SMB_SHUTDOWN_STATE")
	if dir == "" {
		return
	}
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stop()
	r, adapter, table, _ := protectedResourcesAt(t, dir)
	deleteOnClose := os.Getenv("S3_SMB_SHUTDOWN_DELETE_ON_CLOSE") == "true"
	base, baseName := shutdownOpen(t, adapter, table, "delete-base", true, deleteOnClose)
	shutdownOpen(t, adapter, table, "stream-base", true, false)
	stream, streamName := shutdownOpen(t, adapter, table, "stream-base:AFP_Resource:$DATA", false, deleteOnClose)
	shutdownOpen(t, adapter, table, "stream-base:keep:$DATA", false, false)
	if !deleteOnClose {
		for _, selected := range []struct {
			open state.Open
			name smb.Name
		}{{base, baseName}, {stream, streamName}} {
			if status := table.SetDelete(selected.open.ID, selected.open.Binding, selected.name, true); status != smb.StatusSuccess {
				t.Fatal(status)
			}
		}
	}
	if actions := table.Disconnect(base.Binding.SessionID); len(actions) != 0 {
		t.Fatalf("disconnect transferred cleanup: %+v", actions)
	}
	var err error
	r.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	r.serveDone = serveDone
	go func() {
		serveDone <- r.server.Serve(ctx, r.listener)
		close(serveDone)
	}()
	backupDone := r.startBackup(ctx)
	if os.Getenv("S3_SMB_SHUTDOWN_FAILURE") == "true" {
		r.protection.Close()
		if err := <-backupDone; !errors.Is(err, backup.ErrUnprotected) {
			t.Fatalf("backup failure: %v", err)
		}
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if err := r.close(); err != nil {
		exitFailure("shutdown failed; terminating with state lock retained", err)
	}
	*r = resources{}
}

func TestShutdownProcessExitAndRestart(t *testing.T) {
	for _, failure := range []bool{false, true} {
		for _, deleteOnClose := range []bool{false, true} {
			t.Run(fmt.Sprintf("protection-failure=%t/delete-on-close=%t", failure, deleteOnClose), func(t *testing.T) {
				dir := t.TempDir()
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownSignalProcess$")
				cmd.Env = append(os.Environ(), "S3_SMB_SHUTDOWN_STATE="+dir,
					fmt.Sprintf("S3_SMB_SHUTDOWN_FAILURE=%t", failure),
					fmt.Sprintf("S3_SMB_SHUTDOWN_DELETE_ON_CLOSE=%t", deleteOnClose))
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(errors.Join(err, out.Close()))
				}
				line, readyErr := bufio.NewReader(out).ReadString('\n')
				if readyErr != nil || line != "ready\n" {
					cancel()
					waitErr := cmd.Wait()
					t.Fatalf("child startup: %q, %v, %v\n%s", line, readyErr, waitErr, stderr.String())
				}
				signalErr := cmd.Process.Signal(syscall.SIGTERM)
				if signalErr != nil {
					cancel()
				}
				err = cmd.Wait()
				if signalErr != nil || ctx.Err() != nil {
					t.Fatalf("child termination: %v, %v, %v\n%s", signalErr, ctx.Err(), err, stderr.String())
				}
				if failure {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !bytes.Contains(stderr.Bytes(), []byte("remove closed object")) {
						t.Fatalf("failed protection exit: %v\n%s", err, stderr.String())
					}
				} else if err != nil {
					t.Fatalf("protected shutdown exit: %v\n%s", err, stderr.String())
				}
				inspectShutdownRestart(t, dir, failure)
			})
		}
	}
}

func inspectShutdownRestart(t *testing.T, dir string, failure bool) {
	t.Helper()
	conf := meta.DefaultConf()
	conf.ReadOnly = true
	m, err := storage.OpenMetadata(filepath.Join(dir, "metadata.db"), conf)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Error(err)
		}
	}()
	var ino meta.Ino
	var attr meta.Attr
	want := syscall.ENOENT
	if failure {
		want = 0
	}
	if eno := m.Lookup(meta.Background(), meta.RootInode, "delete-base", &ino, &attr, true); eno != want {
		t.Fatalf("base deletion after restart: %v, want %v", eno, want)
	}
	if eno := m.Lookup(meta.Background(), meta.RootInode, "stream-base", &ino, &attr, true); eno != 0 {
		t.Fatalf("stream deletion removed base: %v", eno)
	}
	var data []byte
	if !failure {
		if eno := m.GetXattr(meta.Background(), ino, "AFP_Resource", &data); eno != syscall.ENODATA {
			t.Fatalf("stream deletion after restart: %v", eno)
		}
	}
	if eno := m.GetXattr(meta.Background(), ino, "keep", &data); eno != 0 || string(data) != "accepted before shutdown" {
		t.Fatalf("unrelated stream after restart: %q, %v", data, eno)
	}
	lock, err := lockState(dir)
	if err != nil {
		t.Fatalf("process left its state lock held: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
