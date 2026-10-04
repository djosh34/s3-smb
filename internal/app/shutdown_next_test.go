//go:build smbnext

// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
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

func protectedResources(t *testing.T) (*resources, *smbfs.FS, *state.Table, string) {
	t.Helper()
	return protectedResourcesAt(t, t.TempDir())
}

func protectedResourcesAt(t *testing.T, dir string) (*resources, *smbfs.FS, *state.Table, string) {
	t.Helper()
	p, err := backup.NewProtection(time.Hour, time.Minute, 14)
	if err != nil {
		t.Fatal(err)
	}
	r := &resources{protection: p}
	t.Cleanup(func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	})
	r.lock, err = lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "metadata.db")
	conf := meta.DefaultConf()
	conf.NoBGJob, conf.MaxDeletes, conf.CheckMaintenance = true, 0, p.Check
	r.metadata, err = storage.OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	format, err := storage.NewFormat("app-shutdown-test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.metadata.Init(format, false); err != nil {
		t.Fatal(err)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o770}
	if eno := r.metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	remote := t.TempDir() + "/"
	raw, err := object.CreateStorage("file", remote, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	r.raw = raw
	r.manager, err = backup.New(r.metadata, startupStore{raw, remote}, backup.Options{
		StateDir: dir, DatabasePath: path, Interval: time.Hour, Timeout: time.Minute, Attempts: 1, Protection: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.manager.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	r.runtime, err = storage.OpenFilesystem(r.metadata, raw, format, "/unusable", &zero, p.Check)
	if err != nil {
		t.Fatal(err)
	}
	r.session = true
	if err := r.metadata.NewSession(true); err != nil {
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
	return r, adapter, table, path
}

func shutdownOpen(t *testing.T, adapter *smbfs.FS, table *state.Table, name string, durable, deleteOnClose bool) (state.Open, smb.Name) {
	t.Helper()
	resolved, err := adapter.Lookup(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Exists {
		resolved, err = adapter.Create(t.Context(), resolved.Name, smb.KindFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	handle, err := adapter.Open(t.Context(), resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	request := state.OpenRequest{
		Object: resolved.Object, Binding: state.Binding{SessionID: 1, TreeID: 1},
		GrantedAccess: 0x10003, Sharing: 7, ClientGUID: state.GUID{1}, User: "backup", Share: "Backups",
	}
	if !durable {
		request.Binding.SessionID = 2
	}
	grant := state.Grant{Handle: handle, DeleteName: resolved.Name, DeleteOnClose: deleteOnClose}
	if durable {
		request.CreateGUID = state.GUID{byte(resolved.Object.Inode)}
		grant.Lease = state.Lease{ClientGUID: request.ClientGUID, Key: state.GUID{byte(resolved.Object.Inode)}, State: smb.LeaseRead | smb.LeaseHandle}
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
	if n, err := adapter.WriteAt(t.Context(), handle, data, 0); err != nil || n != len(data) {
		t.Fatal(n, err)
	}
	if err := adapter.Flush(t.Context(), handle, smb.SyncFull); err != nil {
		t.Fatal(err)
	}
	return open, resolved.Name
}

func TestProtectionFailureWithPendingDeletion(t *testing.T) {
	r, adapter, table, path := protectedResources(t)
	base, baseName := shutdownOpen(t, adapter, table, "file", true, false)
	stream, streamName := shutdownOpen(t, adapter, table, "file:AFP_Resource:$DATA", false, false)
	for _, selected := range []struct {
		open state.Open
		name smb.Name
	}{{base, baseName}, {stream, streamName}} {
		if status := table.SetDelete(selected.open.ID, selected.open.Binding, selected.name, true); status != smb.StatusSuccess {
			t.Fatal(status)
		}
	}
	if actions := table.Disconnect(base.Binding.SessionID); len(actions) != 0 {
		t.Fatalf("disconnect transferred cleanup: %+v", actions)
	}
	r.protection.Close()
	done := r.startBackup(t.Context())
	select {
	case err := <-done:
		if !errors.Is(err, backup.ErrUnprotected) {
			t.Fatalf("backup protection failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backup loop did not report closed protection")
	}
	if err := r.close(); !errors.Is(err, smb.ErrIO) {
		t.Fatalf("pending base deletion failure was not reported: %v", err)
	}
	if err := r.protection.Check(); !errors.Is(err, backup.ErrUnprotected) {
		t.Fatalf("shutdown reopened protection: %v", err)
	}
	for _, open := range []state.Open{base, stream} {
		if _, err := adapter.ReadAt(t.Context(), open.Handle, make([]byte, 1), 0); !errors.Is(err, smb.ErrInvalidHandle) {
			t.Fatalf("failed shutdown did not drain handle: %v", err)
		}
	}
	resolved, err := adapter.Lookup(t.Context(), "file")
	if err != nil || !resolved.Exists {
		t.Fatalf("failed deletion changed the base namespace: %+v, %v", resolved, err)
	}
	other, err := lockState(filepath.Dir(path))
	if err == nil {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("failed shutdown released the state lock")
	}
	// Shutdown finished all opens despite its deletion error. Release the test's
	// remaining resources without repeating the server's recorded failure.
	r.server = nil
}

func TestBackupSurvivesServiceCancellation(t *testing.T) {
	r, _, _, _ := protectedResources(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := r.startBackup(ctx)
	cancel()
	select {
	case err := <-done:
		t.Fatalf("service cancellation stopped backup before open drain: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := r.protection.Check(); err != nil {
		t.Fatalf("service cancellation revoked deletion protection: %v", err)
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if err := r.protection.Check(); !errors.Is(err, backup.ErrUnprotected) {
		t.Fatalf("shutdown left protection running: %v", err)
	}
	*r = resources{}
}
