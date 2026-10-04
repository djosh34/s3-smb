// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
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
	capacity := int64(0)
	runtime, err := storage.OpenFilesystem(m, blob, format, t.TempDir(), &capacity, func() error { return ErrUnprotected })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := m.NewSession(true); err != nil {
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
	t.Cleanup(func() {
		if err := adapter.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return adapter
}

func TestPeriodicBackupWithLiveAdapterMutations(t *testing.T) {
	m, format := newMetadata(t)
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0700}
	if eno := m.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	blob := newStore(t)
	adapter := openBackupAdapter(t, m, format, blob, m.path)
	ctx := context.Background()
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
	closeErr := adapter.Close(ctx, handle)
	if n != len(data) || writeErr != nil || closeErr != nil {
		t.Fatalf("write=%d, errors=%v", n, errors.Join(writeErr, closeErr))
	}
	attributes := uint32(0x22)
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := adapter.SetAttr(ctx, file.Object, smb.AttrChange{Attributes: &attributes, Created: &created}); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	const interval = time.Second
	protection, err := NewProtection(interval, 3*time.Second, 14)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(m, blob, Options{StateDir: state, DatabasePath: m.path, Interval: interval, Timeout: 3 * time.Second, Protection: protection})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Wait)
	first, err := manager.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	var latest Receipt
	if !t.Run("periodic", func(t *testing.T) {
		runCtx, cancel := context.WithCancel(ctx)
		runDone := make(chan error, 1)
		mutationCtx, stopMutations := context.WithCancel(ctx)
		mutationDone := make(chan error, 1)
		go func() { runDone <- manager.Run(runCtx) }()
		go func() {
			mutationDone <- mutateDuringBackup(mutationCtx, adapter, directory.Object.Inode)
		}()
		t.Cleanup(func() {
			stopMutations()
			if err := <-mutationDone; err != nil {
				t.Error(err)
			}
			cancel()
			if err := <-runDone; !errors.Is(err, context.Canceled) {
				t.Errorf("backup loop: %v", err)
			}
			manager.Wait()
		})

		deadline := time.Now().Add(8 * time.Second)
		for {
			encoded, err := os.ReadFile(filepath.Join(state, "backup-receipt.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &latest); err != nil {
				t.Fatal(err)
			}
			if !latest.Snapshot.Before(first.Snapshot.Add(2 * interval)) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("two periodic backups did not finish")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if latest.UUID != format.UUID || latest.Key == first.Key {
			t.Fatalf("periodic receipt: %+v", latest)
		}
		if ok, err := manager.Reuse(ctx); err != nil || !ok {
			t.Fatalf("reuse during live mutations: %v, %v", ok, err)
		}
	}) {
		return
	}
	saved, err := Inspect(ctx, blob, latest.Key, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !SameVolume(saved, format) {
		t.Fatal("snapshot changed volume identity")
	}
	database := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Recover(ctx, blob, latest.Key, database, t.TempDir(), format); err != nil {
		t.Fatal(err)
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	restored, err := storage.OpenMetadata(database, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	if _, err := restored.Load(true); err != nil {
		t.Fatal(err)
	}
	recovered := openBackupAdapter(t, restored, format, blob, database)
	entry, err := recovered.Lookup(ctx, "saved/file")
	if err != nil || !entry.Exists || entry.Object != file.Object || entry.Attr.Attributes != attributes || !entry.Attr.Created.Equal(created) {
		t.Fatalf("restored file: %+v, %v", entry, err)
	}
	handle, err = recovered.Open(ctx, entry.Object, smb.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(data))
	n, readErr := recovered.ReadAt(ctx, handle, got, 0)
	closeErr = recovered.Close(ctx, handle)
	if n != len(data) || readErr != nil || closeErr != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored data=%q, n=%d, errors=%v", got, n, errors.Join(readErr, closeErr))
	}
}

func mutateDuringBackup(ctx context.Context, adapter smb.Storage, parent smb.Inode) (err error) {
	work := context.Background()
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
	for i := 0; ctx.Err() == nil; i++ {
		attributes := uint32(0x20 | (i & 3))
		if err := adapter.SetAttr(work, objects[i%len(objects)], smb.AttrChange{Attributes: &attributes}); err != nil {
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

func TestReuseReportsMetadataReadFailure(t *testing.T) {
	m, _ := newMetadata(t)
	blob, state := newStore(t), t.TempDir()
	manager := newManager(t, m, blob, state, time.Now, 5*time.Second)
	t.Cleanup(manager.Wait)
	if _, err := manager.Backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	path := filepath.Join(t.TempDir(), "unformatted.db")
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	unformatted, err := storage.OpenMetadata(path, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unformatted.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	restarted := newManager(t, &testMetadata{unformatted, path}, blob, state, time.Now, 5*time.Second)
	if ok, err := restarted.Reuse(context.Background()); ok || err == nil {
		t.Fatalf("reuse with unreadable format: %v, %v", ok, err)
	}
}
