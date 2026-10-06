// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/engine"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

// openStore opens the engine on dir and the S3 server at endpoint.
func openStore(t *testing.T, dir, endpoint string) *engine.Engine {
	t.Helper()
	bucket, err := engine.NewBucket(engine.BucketOptions{
		Endpoint: endpoint, Region: "us-east-1", Bucket: "bucket",
		AccessKey: "synthetic-access", SecretKey: "synthetic-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.Open(t.Context(), engine.Options{Bucket: bucket, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// shutdownResources holds the folder lock in dir, opens the engine and the
// SMB server, as serve does.
func shutdownResources(t *testing.T, dir, endpoint string) (*resources, *state.Table) {
	t.Helper()
	r := &resources{}
	t.Cleanup(func() {
		if e := r.close(t.Context()); e != nil {
			t.Error(e)
		}
	})
	var err error
	if r.lock, err = lockState(dir); err != nil {
		t.Fatal(err)
	}
	r.store = openStore(t, dir, endpoint)
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	r.server, err = server.New(server.Options{
		Storage: r.store, State: table, Logger: slog.Default(), Now: time.Now,
		Account: auth.Account{User: "backup", Password: "password"}, ShareName: "Backups",
		ServerName: "s3-smb", ServerGUID: [16]byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, table
}

// shutdownOpen opens name as an SMB client would and writes to it. A durable
// open survives its session; a plain one belongs to another session.
func shutdownOpen(t *testing.T, storage smb.Storage, table *state.Table, name string, durable, deleteOnClose bool) (state.Open, smb.Name) {
	t.Helper()
	resolved, err := storage.Lookup(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Exists {
		if resolved, err = storage.Create(t.Context(), resolved.Name, smb.KindFile); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	request := state.OpenRequest{
		Object: resolved.Object, Binding: state.Binding{SessionID: 2, TreeID: 1},
		GrantedAccess: 0x10003, Sharing: 7, ClientGUID: state.GUID{1}, User: "backup", Share: "Backups",
	}
	grant := state.Grant{Handle: handle, DeleteName: resolved.Name, DeleteOnClose: deleteOnClose}
	if durable {
		request.Binding.SessionID = 1
		var key state.GUID
		binary.LittleEndian.PutUint64(key[:], uint64(resolved.Object))
		request.CreateGUID = key
		grant.Lease = state.Lease{ClientGUID: request.ClientGUID, Key: key, State: smb.LeaseRead | smb.LeaseHandle}
		grant.DurableTimeout = time.Minute
	}
	reservation, status := table.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(errors.Join(fmt.Errorf("reserve open: %#x", status), storage.Close(t.Context(), handle)))
	}
	open, status := table.Commit(reservation, grant)
	if status != smb.StatusSuccess {
		t.Fatal(errors.Join(fmt.Errorf("commit open: %#x", status), storage.Close(t.Context(), handle)))
	}
	data := []byte("accepted before shutdown")
	if n, e := storage.WriteAt(t.Context(), handle, data, 0); e != nil || n != len(data) {
		t.Fatal(n, e)
	}
	if err = storage.Flush(t.Context(), handle, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	return open, resolved.Name
}

// TestShutdownSignalProcess is the child of TestShutdownProcessExitAndRestart.
// It holds opens with pending deletions in its working directory and shuts
// down on SIGTERM.
func TestShutdownSignalProcess(t *testing.T) {
	endpoint := os.Getenv("S3_SMB_SHUTDOWN_CHILD")
	if endpoint == "" {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stop()
	r, table := shutdownResources(t, dir, endpoint)
	deleteOnClose := os.Getenv("S3_SMB_SHUTDOWN_DELETE_ON_CLOSE") == "true"
	open, name := shutdownOpen(t, r.store, table, "delete", true, deleteOnClose)
	shutdownOpen(t, r.store, table, "keep", false, false)
	if !deleteOnClose {
		if status := table.SetDelete(open.ID, open.Binding, name, true); status != smb.StatusSuccess {
			t.Fatal(status)
		}
	}
	if actions := table.Disconnect(open.Binding.SessionID); len(actions) != 0 {
		t.Fatalf("disconnect transferred cleanup: %+v", actions)
	}
	if r.listener, err = new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	r.serveDone = serveDone
	go func() {
		serveDone <- r.server.Serve(ctx, r.listener)
		close(serveDone)
	}()
	if _, err = fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	err = r.close(ctx)
	// close ran once; the cleanup must not run it again.
	*r = resources{}
	if err != nil {
		t.Fatal(err)
	}
}

// runShutdownChild starts TestShutdownSignalProcess in dir on the S3 server
// at endpoint, stops it with SIGTERM once it is ready and returns its output
// and exit error.
func runShutdownChild(t *testing.T, dir, endpoint string, deleteOnClose bool) ([]byte, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestShutdownSignalProcess$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "S3_SMB_SHUTDOWN_CHILD="+endpoint,
		fmt.Sprintf("S3_SMB_SHUTDOWN_DELETE_ON_CLOSE=%t", deleteOnClose))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdout := bufio.NewReader(out)
	if line, e := stdout.ReadString('\n'); e != nil || line != "ready\n" {
		waitErr := cmd.Wait()
		t.Fatalf("child startup: %q, %v, %v\n%s", line, e, waitErr, stderr.String())
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(errors.Join(err, cmd.Wait()))
	}
	rest, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatal(errors.Join(err, cmd.Wait()))
	}
	err = cmd.Wait()
	return append(rest, stderr.Bytes()...), err
}

// A SIGTERM during open deletions finishes them before exit, so a restart
// sees them, and releases the folder lock.
func TestShutdownProcessExitAndRestart(t *testing.T) {
	for _, deleteOnClose := range []bool{false, true} {
		dir, endpoint := t.TempDir(), smbtest.NewS3(t)
		if output, err := runShutdownChild(t, dir, endpoint, deleteOnClose); err != nil {
			t.Fatalf("delete on close %t: child exit %v\n%s", deleteOnClose, err, output)
		}
		lock, err := lockState(dir)
		if err != nil {
			t.Fatalf("process left its folder lock held: %v", err)
		}
		inspectShutdownRestart(t, openStore(t, dir, endpoint))
		if err = lock.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func inspectShutdownRestart(t *testing.T, e *engine.Engine) {
	t.Helper()
	defer func() {
		if err := e.Shutdown(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	if resolved, err := e.Lookup(t.Context(), "delete"); err != nil || resolved.Exists {
		t.Fatalf("deletion after restart: %+v, %v", resolved, err)
	}
	resolved, err := e.Lookup(t.Context(), "keep")
	if err != nil || !resolved.Exists {
		t.Fatalf("kept file after restart: %+v, %v", resolved, err)
	}
	handle, err := e.Open(t.Context(), resolved.Object, smb.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64)
	n, err := e.ReadAt(t.Context(), handle, data, 0)
	if err = errors.Join(ignoreEOF(err), e.Close(t.Context(), handle)); err != nil || string(data[:n]) != "accepted before shutdown" {
		t.Fatalf("kept file after restart: %q, %v", data[:n], err)
	}
}

func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
