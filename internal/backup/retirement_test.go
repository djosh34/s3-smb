// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/storage"
)

func nativeRuntime(t *testing.T, m meta.Meta, s object.ObjectStorage, f *meta.Format) *storage.Runtime {
	t.Helper()
	capacity := uint64(0)
	r, err := storage.OpenFilesystem(m, s, f, t.TempDir(), &capacity, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, r.Close)
	return r
}

const sliceSize = 1000

// writeNativeSlice writes one slice of sliceSize bytes at offset.
func writeNativeSlice(t *testing.T, m meta.Meta, store chunk.ChunkStore, ino meta.Ino, offset uint32, fill byte) uint64 {
	t.Helper()
	var id uint64
	if st := m.NewSlice(meta.Background(), &id); st != 0 {
		t.Fatal(st)
	}
	w := store.NewWriter(id, 0)
	if n, err := w.WriteAt(bytes.Repeat([]byte{fill}, sliceSize), 0); err != nil || n != sliceSize {
		t.Fatal(n, err)
	}
	if err := w.Finish(sliceSize); err != nil {
		t.Fatal(err)
	}
	slice := meta.Slice{Id: id, Size: sliceSize, Len: sliceSize}
	if st := m.Write(meta.Background(), ino, 0, offset, slice, time.Now()); st != 0 {
		t.Fatal(st)
	}
	return id
}

func readSlice(store chunk.ChunkStore, id uint64) error {
	p := chunk.NewOffPage(sliceSize)
	defer p.Release()
	_, err := store.NewReader(id, sliceSize).ReadAt(context.Background(), p, 0)
	return err
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
		data = append(data, p.Data[:n]...)
		p.Release()
		if err != nil || n != int(s.Len) {
			t.Fatal(n, err)
		}
	}
	return data
}

// The last allocated inode is absent from the tree but still has pending data
// deletion. Restoring only the tree would let a new file reuse that inode and
// lose its data to the old deletion.
func TestPendingDeletionCannotDeleteNewFileAfterRecovery(t *testing.T) {
	// Allow unlink's initial and final transaction checks, then pause the
	// asynchronous data retirement at the maintenance guard.
	var unlinking atomic.Bool
	var checks atomic.Int32
	m, f := newMetadata(t, func() error {
		if unlinking.Load() && checks.Add(1) > 2 {
			return ErrUnprotected
		}
		return nil
	})
	s := newStore(t)
	runtime := nativeRuntime(t, m, s, f)
	createInode(t, m, "kept")
	old := createInode(t, m, "deleted")
	writeNativeSlice(t, m, runtime.Store, old, 0, 'o')
	if st := m.Close(meta.Background(), old); st != 0 {
		t.Fatal(st)
	}
	unlinking.Store(true)
	if st := m.Unlink(meta.Background(), meta.RootInode, "deleted", true); st != 0 {
		t.Fatal(st)
	}
	if queryCount(t, m.path, "jfs_delfile") != 1 {
		t.Fatal("fixture did not keep the pending deletion")
	}
	r := backupOnce(t, m, s)
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Recover(context.Background(), s, r.Key, path, t.TempDir(), f); err != nil {
		t.Fatal(err)
	}
	if queryCount(t, path, "jfs_delfile") != 1 {
		t.Fatal("recovery dropped the pending deletion")
	}
	// Without a session, JuiceFS deletes slices synchronously.
	restored := openMetadata(t, path, false, 1)
	freshRuntime := nativeRuntime(t, restored, s, f)
	fresh := createInode(t, restored, "new-file")
	if fresh <= old {
		t.Fatalf("reused pending-deletion inode %d as %d", old, fresh)
	}
	writeNativeSlice(t, restored, freshRuntime.Store, fresh, 0, 'n')
	var cleaned int
	err := restored.ScanDeletedObject(meta.Background(), nil, nil, nil, func(ino meta.Ino, _ uint64, _ int64) (bool, error) {
		if ino != old {
			t.Errorf("unexpected pending inode %d, want %d", ino, old)
		}
		cleaned++
		return true, nil
	})
	if err != nil || cleaned != 1 || queryCount(t, path, "jfs_delfile") != 0 {
		t.Fatal("pending deletion did not run", cleaned, err)
	}
	if got := readNativeFile(t, restored, freshRuntime.Store, fresh); !bytes.Equal(got, bytes.Repeat([]byte{'n'}, sliceSize)) {
		t.Fatalf("pending deletion damaged new data: %q", got)
	}
}

// Compaction retires the old slices only after the trash period. Both the
// delayed group and its reference counts must survive recovery.
func TestCompactedSlicesRetireAfterSnapshotRecovery(t *testing.T) {
	m, f := newMetadata(t)
	s := newStore(t)
	runtime := nativeRuntime(t, m, s, f)
	ino := createInode(t, m, "compacted")
	oldA := writeNativeSlice(t, m, runtime.Store, ino, 0, 'a')
	oldB := writeNativeSlice(t, m, runtime.Store, ino, sliceSize, 'b')
	if st := m.Compact(meta.Background(), ino, 1, func() {}, func() {}); st != 0 {
		t.Fatal(st)
	}
	if queryCount(t, m.path, "jfs_delslices") != 1 {
		t.Fatal("compaction did not delay the old slices")
	}
	r := backupOnce(t, m, s)
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Recover(context.Background(), s, r.Key, path, t.TempDir(), f); err != nil {
		t.Fatal(err)
	}
	if queryCount(t, path, "jfs_delslices") != 1 || queryCount(t, path, "jfs_chunk_ref") != 3 {
		t.Fatal("snapshot dropped delayed slices or their reference counts")
	}
	// Without a session, JuiceFS deletes slices synchronously.
	restored := openMetadata(t, path, false, 1)
	freshRuntime := nativeRuntime(t, restored, s, f)
	clock := time.Now()
	clean := func(_ []meta.Slice, deleted int64) (bool, error) {
		return deleted < clock.Add(-time.Duration(f.TrashDays)*24*time.Hour).Unix(), nil
	}
	if err := restored.ScanDeletedObject(meta.Background(), clean, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if queryCount(t, path, "jfs_delslices") != 1 {
		t.Fatal("compacted slices retired before the trash period")
	}
	for _, id := range []uint64{oldA, oldB} {
		if err := readSlice(freshRuntime.Store, id); err != nil {
			t.Fatal("old slice disappeared before the trash period:", err)
		}
	}
	clock = clock.Add(time.Duration(f.TrashDays+1) * 24 * time.Hour)
	if err := restored.ScanDeletedObject(meta.Background(), clean, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if queryCount(t, path, "jfs_delslices") != 0 || queryCount(t, path, "jfs_chunk_ref") != 1 {
		t.Fatal("delayed slices were not retired after the trash period")
	}
	for _, id := range []uint64{oldA, oldB} {
		if readSlice(freshRuntime.Store, id) == nil {
			t.Fatal("old compacted slice is still readable", id)
		}
		key := fmt.Sprintf("chunks/0/0/%d_0_%d", id, sliceSize)
		if _, err := s.Head(context.Background(), key); !os.IsNotExist(err) {
			t.Fatal("old compacted object was not deleted", key, err)
		}
	}
	want := append(bytes.Repeat([]byte{'a'}, sliceSize), bytes.Repeat([]byte{'b'}, sliceSize)...)
	if got := readNativeFile(t, restored, freshRuntime.Store, ino); !bytes.Equal(got, want) {
		t.Fatal("compacted live data changed after old slices retired")
	}
}
