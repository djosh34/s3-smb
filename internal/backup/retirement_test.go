// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/storage"
)

func nativeRuntime(t *testing.T, m meta.Meta, s object.ObjectStorage, f *meta.Format, cache string, capacity int64) *storage.Runtime {
	t.Helper()
	r, err := storage.OpenFilesystem(m, s, f, cache, &capacity, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func writeNativeSlice(t *testing.T, m meta.Meta, store chunk.ChunkStore, ino meta.Ino, offset uint32, data []byte) uint64 {
	t.Helper()
	var id uint64
	if st := m.NewSlice(meta.Background(), &id); st != 0 {
		t.Fatal(st)
	}
	w := store.NewWriter(id, 0)
	if n, err := w.WriteAt(data, 0); err != nil || n != len(data) {
		t.Fatal(n, err)
	}
	if err := w.Finish(len(data)); err != nil {
		t.Fatal(err)
	}
	slice := meta.Slice{Id: id, Size: uint32(len(data)), Len: uint32(len(data))}
	if st := m.Write(meta.Background(), ino, 0, offset, slice, time.Now()); st != 0 {
		t.Fatal(st)
	}
	return id
}

func readNativeFile(t *testing.T, m meta.Meta, store chunk.ChunkStore, ino meta.Ino) []byte {
	t.Helper()
	var slices []meta.Slice
	if st := m.Read(meta.Background(), ino, 0, &slices); st != 0 {
		t.Fatal(st)
	}
	var data []byte
	for _, s := range slices {
		p := chunk.NewOffPage(int(s.Len))
		n, err := store.NewReader(s.Id, int(s.Size)).ReadAt(context.Background(), p, int(s.Off))
		if err != nil || n != int(s.Len) {
			p.Release()
			t.Fatal(n, err)
		}
		data = append(data, p.Data...)
		p.Release()
	}
	return data
}

// Regression for #92: the last allocated inode is absent from the tree but
// still has pending data deletion. Restoring only the tree would reuse it.
func TestPendingDeletionCannotDeleteNewFileAfterRecovery(t *testing.T) {
	m, f := newMetadata(t)
	s := newStore(t)
	runtime := nativeRuntime(t, m, s, f, t.TempDir(), 0)
	createInode(t, m, "kept")
	old := createInode(t, m, "deleted")
	writeNativeSlice(t, m, runtime.Store, old, 0, []byte("old file"))
	if st := m.Close(meta.Background(), old); st != 0 {
		t.Fatal(st)
	}
	if st := m.Unlink(meta.Background(), meta.RootInode, "deleted", true); st != 0 {
		t.Fatal(st)
	}
	if queryCount(t, m.path, "jfs_delfile") != 1 {
		t.Fatal("fixture did not retain pending deletion")
	}
	r, err := newManager(t, m, s, t.TempDir(), time.Now, time.Minute).Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(context.Background(), s, r.Key, path, f); err != nil {
		t.Fatal(err)
	}
	restored := recoveredMetadata(t, path, false, 1)
	freshRuntime := nativeRuntime(t, restored, s, f, t.TempDir(), 0)
	if err = restored.NewSession(true); err != nil {
		t.Fatal(err)
	}
	fresh := createInode(t, restored, "new-file")
	if fresh <= old {
		t.Fatalf("reused pending-deletion inode %d as %d", old, fresh)
	}
	want := []byte("new file survives old pending deletion")
	writeNativeSlice(t, restored, freshRuntime.Store, fresh, 0, want)
	var cleaned int
	err = restored.ScanDeletedObject(meta.Background(), nil, nil, nil, func(ino meta.Ino, _ uint64, _ int64) (bool, error) {
		if ino != old {
			t.Errorf("unexpected pending inode %d, want %d", ino, old)
		}
		cleaned++
		return true, nil // Make the restored retirement work eligible now.
	})
	if err != nil || cleaned != 1 || queryCount(t, path, "jfs_delfile") != 0 {
		t.Fatal("pending deletion was not executed", cleaned, err)
	}
	if got := readNativeFile(t, restored, freshRuntime.Store, fresh); !bytes.Equal(got, want) {
		t.Fatalf("pending deletion damaged new data: %q", got)
	}
}

// Regression for #114: native compaction retires slices only after the trash
// period. Both the delayed group and its reference counts must survive recovery.
func TestCompactedSlicesRetireAfterSnapshotRecovery(t *testing.T) {
	m, f := newMetadata(t)
	s := newStore(t)
	runtime := nativeRuntime(t, m, s, f, t.TempDir(), 0)
	ino := createInode(t, m, "compacted")
	a, b := bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 1000)
	oldA := writeNativeSlice(t, m, runtime.Store, ino, 0, a)
	oldB := writeNativeSlice(t, m, runtime.Store, ino, uint32(len(a)), b)
	if st := m.Compact(meta.Background(), ino, 1, func() {}, func() {}); st != 0 {
		t.Fatal(st)
	}
	if queryCount(t, m.path, "jfs_delslices") != 1 {
		t.Fatal("native compaction did not retain delayed slices")
	}
	r, err := newManager(t, m, s, t.TempDir(), time.Now, time.Minute).Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err = Recover(context.Background(), s, r.Key, path, f); err != nil {
		t.Fatal(err)
	}
	if queryCount(t, path, "jfs_delslices") != 1 || queryCount(t, path, "jfs_chunk_ref") != 3 {
		t.Fatal("snapshot dropped delayed slices or their reference counts")
	}
	restored := recoveredMetadata(t, path, false, 1)
	freshRuntime := nativeRuntime(t, restored, s, f, t.TempDir(), 0)
	if err = restored.NewSession(true); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	clean := func(_ []meta.Slice, deleted int64) (bool, error) {
		return deleted < clock.Add(-time.Duration(f.TrashDays)*24*time.Hour).Unix(), nil
	}
	if err = restored.ScanDeletedObject(meta.Background(), clean, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if queryCount(t, path, "jfs_delslices") != 1 {
		t.Fatal("compacted slices retired before trash period")
	}
	for _, id := range []uint64{oldA, oldB} {
		p := chunk.NewOffPage(1000)
		_, err = freshRuntime.Store.NewReader(id, 1000).ReadAt(context.Background(), p, 0)
		p.Release()
		if err != nil {
			t.Fatal("old slice disappeared before trash period", err)
		}
	}
	clock = clock.Add(time.Duration(f.TrashDays+1) * 24 * time.Hour)
	if err = restored.ScanDeletedObject(meta.Background(), clean, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for queryCount(t, path, "jfs_chunk_ref") != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if queryCount(t, path, "jfs_delslices") != 0 || queryCount(t, path, "jfs_chunk_ref") != 1 {
		t.Fatal("delayed slices were not retired after trash period")
	}
	if err = restored.CloseSession(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{oldA, oldB} {
		p := chunk.NewOffPage(1000)
		_, err = freshRuntime.Store.NewReader(id, 1000).ReadAt(context.Background(), p, 0)
		p.Release()
		if err == nil {
			t.Fatal("old compacted slice is still readable", id)
		}
		key := fmt.Sprintf("chunks/0/0/%d_0_1000", id)
		if _, err = s.Head(context.Background(), key); !os.IsNotExist(err) {
			t.Fatal("old compacted object was not deleted", key, err)
		}
	}
	if got := readNativeFile(t, restored, freshRuntime.Store, ino); !bytes.Equal(got, append(a, b...)) {
		t.Fatal("compacted live data changed after old slices retired")
	}
}
