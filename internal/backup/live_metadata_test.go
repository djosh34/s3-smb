// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

func openBackupAdapter(t *testing.T, m meta.Meta, format *meta.Format, blob object.ObjectStorage, database string) *smbfs.FS {
	t.Helper()
	capacity := uint64(0)
	runtime, err := storage.OpenFilesystem(m, blob, format, t.TempDir(), &capacity, func() error { return ErrUnprotected })
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, runtime.Close)
	if err = m.NewSession(true); err != nil {
		t.Fatal(err)
	}
	barrier, err := smbfs.NewMetadataBarrier(database)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := smbfs.New(smbfs.Options{Filesystem: runtime.FS, Barrier: barrier, MetadataPath: database, Config: runtime.Config, Store: runtime.Store})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, adapter.Shutdown)
	return adapter
}

// Backups and Reuse must not read the live JuiceFS format while SMB changes
// files, or the race detector reports a data race. The last backup restores
// the data and attributes written through the adapter.
func TestBackupDuringLiveAdapterMutations(t *testing.T) {
	ctx := context.Background()
	m, format := newMetadata(t)
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o700}
	if eno := m.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	blob := newStore(t)
	adapter := openBackupAdapter(t, m, format, blob, m.path)
	directory, err := adapter.Create(ctx, smb.Name{Parent: smb.Inode(meta.RootInode), Base: "saved"}, smb.KindDirectory)
	if err != nil {
		t.Fatal(err)
	}
	file, err := adapter.Create(ctx, smb.Name{Parent: directory.Object.Inode, Base: "file"}, smb.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("durable backup data")
	handle, err := adapter.Open(ctx, file.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := adapter.WriteAt(ctx, handle, data, 0)
	if err = errors.Join(writeErr, adapter.Close(ctx, handle)); err != nil || n != len(data) {
		t.Fatalf("write=%d: %v", n, err)
	}
	attributes := uint32(0x22)
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err = adapter.SetAttr(ctx, file.Object, smb.AttrChange{Attributes: &attributes, Created: &created}); err != nil {
		t.Fatal(err)
	}

	clock := newClock()
	mgr := newManager(t, m, blob, t.TempDir(), clock.now, time.Minute)
	mutationCtx, stopMutations := context.WithCancel(ctx)
	mutationDone := make(chan error, 1)
	go func() { mutationDone <- mutate(mutationCtx, adapter, directory.Object.Inode) }()
	var last Receipt
	for range 3 {
		if last, err = mgr.Backup(ctx); err != nil {
			break
		}
		var ok bool
		if ok, err = mgr.Reuse(ctx); err != nil || !ok {
			err = errors.Join(err, errors.New("reuse refused the last backup"))
			break
		}
		clock.add(time.Hour)
	}
	stopMutations()
	if err = errors.Join(err, <-mutationDone); err != nil {
		t.Fatal(err)
	}
	mgr.Wait()

	saved, err := Inspect(ctx, blob, last.Key, t.TempDir())
	if err != nil || !SameVolume(saved, format) {
		t.Fatalf("snapshot changed volume identity: %v", err)
	}
	database := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(ctx, blob, last.Key, database, t.TempDir(), format); err != nil {
		t.Fatal(err)
	}
	recovered := openBackupAdapter(t, openMetadata(t, database, false, 0), format, blob, database)
	entry, err := recovered.Lookup(ctx, "saved/file")
	if err != nil || !entry.Exists || entry.Object != file.Object || entry.Attr.Attributes != attributes || !entry.Attr.Created.Equal(created) {
		t.Fatalf("restored file: %+v, %v", entry, err)
	}
	if handle, err = recovered.Open(ctx, entry.Object, smb.AccessRead); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(data))
	n, readErr := recovered.ReadAt(ctx, handle, got, 0)
	if err = errors.Join(readErr, recovered.Close(ctx, handle)); err != nil || n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("restored data=%q, n=%d: %v", got, n, err)
	}
}

// mutate changes attributes and data under parent until ctx ends.
func mutate(ctx context.Context, adapter smb.Storage, parent smb.Inode) (err error) {
	work := context.WithoutCancel(ctx)
	directory, err := adapter.Create(work, smb.Name{Parent: parent, Base: "live"}, smb.KindDirectory)
	if err != nil {
		return err
	}
	file, err := adapter.Create(work, smb.Name{Parent: directory.Object.Inode, Base: "file"}, smb.KindFile)
	if err != nil {
		return err
	}
	handle, err := adapter.Open(work, file.Object, smb.AccessWrite)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, adapter.Close(work, handle)) }()
	objects := []smb.ObjectKey{directory.Object, file.Object}
	for i := uint32(0); ctx.Err() == nil; i++ {
		attributes := 0x20 | i&3
		if err := adapter.SetAttr(work, objects[i%2], smb.AttrChange{Attributes: &attributes}); err != nil {
			return err
		}
		if _, err := adapter.WriteAt(work, handle, []byte("live data"), 0); err != nil {
			return err
		}
		if i%16 == 0 {
			if err := adapter.Flush(work, handle, smb.SyncData); err != nil {
				return err
			}
		}
	}
	return nil
}
