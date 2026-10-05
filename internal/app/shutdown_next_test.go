//go:build smbnext

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
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

// protectedResources opens a writable volume in dir with a metadata backup,
// deletion protection and the new SMB server, as serve does.
func protectedResources(t *testing.T, dir string) (*resources, *smbfs.FS, *state.Table) {
	t.Helper()
	p, err := backup.NewProtection(time.Hour, time.Minute, 14)
	if err != nil {
		t.Fatal(err)
	}
	r := &resources{protection: p}
	t.Cleanup(func() {
		if e := r.close(t.Context()); e != nil {
			t.Error(e)
		}
	})
	if r.lock, err = lockState(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "metadata.db")
	conf := meta.DefaultConf()
	conf.NoBGJob, conf.MaxDeletes, conf.CheckMaintenance = true, 0, p.Check
	if r.metadata, err = storage.OpenMetadata(path, conf); err != nil {
		t.Fatal(err)
	}
	format, err := storage.NewFormat("app-shutdown-test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.metadata.Init(format, false); err != nil {
		t.Fatal(err)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o770}
	if eno := r.metadata.SetAttr(meta.WrapContext(t.Context()), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	remote := filepath.Join(dir, "remote") + "/"
	if r.raw, err = object.CreateStorage("file", remote, "", "", ""); err != nil {
		t.Fatal(err)
	}
	r.manager, err = backup.New(startupStore{r.raw, remote}, backup.Options{
		StateDir: dir, DatabasePath: path, Interval: time.Hour, Timeout: time.Minute, Protection: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.manager.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	if r.runtime, err = storage.OpenFilesystem(r.metadata, r.raw, format, "/unusable", &zero, p.Check); err != nil {
		t.Fatal(err)
	}
	r.session = true
	if err = r.metadata.NewSession(true); err != nil {
		t.Fatal(err)
	}
	barrier, err := smbfs.NewMetadataBarrier(path)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := smbfs.New(smbfs.Options{Filesystem: r.runtime.FS, Config: r.runtime.Config, Store: r.runtime.Store, Barrier: barrier, MetadataPath: path})
	if err != nil {
		t.Fatal(err)
	}
	r.adapter = adapter
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	r.server, err = server.New(server.Options{
		Storage: adapter, State: table, Logger: slog.Default(), Now: time.Now,
		Account: auth.Account{User: "backup", Password: "password"}, ShareName: "Backups",
		ServerName: "s3-smb", ServerGUID: [16]byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, adapter, table
}

// shutdownOpen opens name as an SMB client would and writes to it. A durable
// open survives its session; a plain one belongs to another session.
func shutdownOpen(t *testing.T, adapter *smbfs.FS, table *state.Table, name string, durable, deleteOnClose bool) (state.Open, smb.Name) {
	t.Helper()
	resolved, err := adapter.Lookup(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Exists {
		if resolved, err = adapter.Create(t.Context(), resolved.Name, smb.KindFile); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := adapter.Open(t.Context(), resolved.Object, smb.AccessRead|smb.AccessWrite)
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
		binary.LittleEndian.PutUint64(key[:], uint64(resolved.Object.Inode))
		request.CreateGUID = key
		grant.Lease = state.Lease{ClientGUID: request.ClientGUID, Key: key, State: smb.LeaseRead | smb.LeaseHandle}
		grant.DurableTimeout = time.Minute
	}
	reservation, status := table.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(errors.Join(fmt.Errorf("reserve open: %#x", status), adapter.Close(t.Context(), handle)))
	}
	open, status := table.Commit(reservation, grant)
	if status != smb.StatusSuccess {
		t.Fatal(errors.Join(fmt.Errorf("commit open: %#x", status), adapter.Close(t.Context(), handle)))
	}
	data := []byte("accepted before shutdown")
	if n, e := adapter.WriteAt(t.Context(), handle, data, 0); e != nil || n != len(data) {
		t.Fatal(n, e)
	}
	if err = adapter.Flush(t.Context(), handle, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	return open, resolved.Name
}

// TestShutdownSignalProcess is the child of TestShutdownProcessExitAndRestart.
// It holds opens with pending deletions in its working directory and shuts
// down on SIGTERM.
func TestShutdownSignalProcess(t *testing.T) {
	if os.Getenv("S3_SMB_SHUTDOWN_CHILD") != "true" {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stop()
	r, adapter, table := protectedResources(t, dir)
	deleteOnClose := os.Getenv("S3_SMB_SHUTDOWN_DELETE_ON_CLOSE") == "true"
	base, baseName := shutdownOpen(t, adapter, table, "delete-base", true, deleteOnClose)
	shutdownOpen(t, adapter, table, "stream-base", true, false)
	stream, streamName := shutdownOpen(t, adapter, table, "stream-base:AFP_Resource:$DATA", false, deleteOnClose)
	shutdownOpen(t, adapter, table, "stream-base:keep:$DATA", false, false)
	if !deleteOnClose {
		for _, selected := range []struct {
			name smb.Name
			open state.Open
		}{{baseName, base}, {streamName, stream}} {
			if status := table.SetDelete(selected.open.ID, selected.open.Binding, selected.name, true); status != smb.StatusSuccess {
				t.Fatal(status)
			}
		}
	}
	if actions := table.Disconnect(base.Binding.SessionID); len(actions) != 0 {
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
	backupDone := r.startBackup(ctx)
	if os.Getenv("S3_SMB_SHUTDOWN_FAILURE") == "true" {
		r.protection.Close()
		if err = <-backupDone; !errors.Is(err, backup.ErrUnprotected) {
			t.Fatalf("backup failure: %v", err)
		}
	}
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

// runShutdownChild starts TestShutdownSignalProcess in dir, stops it with
// SIGTERM once it is ready and returns its output and exit error.
func runShutdownChild(t *testing.T, dir string, failure, deleteOnClose bool) ([]byte, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestShutdownSignalProcess$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "S3_SMB_SHUTDOWN_CHILD=true",
		fmt.Sprintf("S3_SMB_SHUTDOWN_FAILURE=%t", failure),
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
// sees them. When deletion protection has failed, the deletions are not
// applied and the process exits with a failure.
func TestShutdownProcessExitAndRestart(t *testing.T) {
	for _, failure := range []bool{false, true} {
		for _, deleteOnClose := range []bool{false, true} {
			dir := t.TempDir()
			output, err := runShutdownChild(t, dir, failure, deleteOnClose)
			if failure != (err != nil) || failure && !bytes.Contains(output, []byte("remove closed object")) {
				t.Fatalf("failure %t, delete on close %t: child exit %v\n%s", failure, deleteOnClose, err, output)
			}
			inspectShutdownRestart(t, dir, failure)
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
		if e := m.Shutdown(); e != nil {
			t.Error(e)
		}
	}()
	var ino meta.Ino
	var attr meta.Attr
	ctx := meta.WrapContext(t.Context())
	want := syscall.ENOENT
	if failure {
		want = 0
	}
	if eno := m.Lookup(ctx, meta.RootInode, "delete-base", &ino, &attr, true); eno != want {
		t.Fatalf("base deletion after restart: %v, want %v", eno, want)
	}
	if eno := m.Lookup(ctx, meta.RootInode, "stream-base", &ino, &attr, true); eno != 0 {
		t.Fatalf("stream deletion removed base: %v", eno)
	}
	var data []byte
	if !failure {
		if eno := m.GetXattr(ctx, ino, "AFP_Resource", &data); eno != syscall.ENODATA {
			t.Fatalf("stream deletion after restart: %v", eno)
		}
	}
	if eno := m.GetXattr(ctx, ino, "keep", &data); eno != 0 || string(data) != "accepted before shutdown" {
		t.Fatalf("unrelated stream after restart: %q, %v", data, eno)
	}
	lock, err := lockState(dir)
	if err != nil {
		t.Fatalf("process left its state lock held: %v", err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
}
