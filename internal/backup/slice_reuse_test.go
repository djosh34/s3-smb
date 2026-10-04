// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// Both cache runtimes live in child processes. The parent owns their cache
// directory and waits for process exit to stop all cache workers before cleanup.
// Exiting the old writer models machine loss before recovery wipes the volume.
func TestSliceIDReuseReadsNewBytesThroughDiskCache(t *testing.T) {
	if root := os.Getenv("S3_SMB_SNAPSHOT_CACHE_CHILD"); root != "" {
		runSliceCacheChild(t, root)
		return
	}
	root := os.Getenv("S3_SMB_SNAPSHOT_CACHE_SCENARIO")
	if root == "" {
		root = t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSliceIDReuseReadsNewBytesThroughDiskCache$", "-test.timeout=40s")
		cmd.Env = append(os.Environ(), "S3_SMB_SNAPSHOT_CACHE_SCENARIO="+root)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cache recovery scenario: %v\n%s", err, output)
		}
		return
	}
	m, f := newMetadata(t)
	s := newStore(t)
	mgr := newManager(t, m, s, t.TempDir(), time.Now, time.Minute)
	r, err := mgr.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"source": m.path, "remote": s.dir} {
		if err = os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(root, "cache")
	if err = os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(cache, "unrelated")
	if err = os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSliceIDReuseReadsNewBytesThroughDiskCache$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), "S3_SMB_SNAPSHOT_CACHE_CHILD="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("old writer: %v\n%s", err, output)
	}
	volumeCache := filepath.Join(cache, f.UUID)
	if countCacheFiles(t, volumeCache) == 0 {
		t.Fatal("old writer left no cached slice data")
	}
	idBytes, err := os.ReadFile(filepath.Join(root, "slice-id"))
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := strconv.ParseUint(string(idBytes), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	// Lose the local metadata. The snapshot precedes the child's allocation.
	if err = os.Remove(m.path); err != nil {
		t.Fatal(err)
	}
	if _, err = Recover(context.Background(), s, r.Key, m.path, cache, f); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(volumeCache); !os.IsNotExist(err) {
		t.Fatal("recovery retained stale cache", err)
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "keep" {
		t.Fatal("recovery changed unrelated cache-root files", err)
	}
	restored := recoveredMetadata(t, m.path, false, 0)
	// Write without populating the disk cache, then read through the configured
	// nonempty cache root. A missing wipe would return the child's old bytes.
	writer := nativeRuntime(t, restored, s, f, cache, 0)
	ino := createInode(t, restored, "after-recovery")
	want := bytes.Repeat([]byte("new-byte"), 1024)
	id := writeNativeSlice(t, restored, writer.Store, ino, 0, want)
	if id != oldID {
		t.Fatalf("fixture did not reuse post-snapshot slice ID: old=%d new=%d", oldID, id)
	}
	reader := nativeRuntime(t, restored, s, f, cache, 64<<20)
	if got := readNativeFile(t, restored, reader.Store, ino); !bytes.Equal(got, want) {
		t.Fatal("reused slice returned pre-recovery cached bytes")
	}
}

func runSliceCacheChild(t *testing.T, root string) {
	t.Helper()
	readPath := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m := recoveredMetadata(t, readPath("source"), false, 0)
	f, err := m.Load(true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := object.CreateStorage("file", readPath("remote"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "cache")
	runtime := nativeRuntime(t, m, s, f, cache, 64<<20)
	ino := createInode(t, m, "lost-after-snapshot")
	old := bytes.Repeat([]byte("old-byte"), 1024)
	id := writeNativeSlice(t, m, runtime.Store, ino, 0, old)
	if got := readNativeFile(t, m, runtime.Store, ino); !bytes.Equal(got, old) {
		t.Fatal("old writer did not populate readable data")
	}
	deadline := time.Now().Add(5 * time.Second)
	for countCacheFiles(t, filepath.Join(cache, f.UUID)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if countCacheFiles(t, filepath.Join(cache, f.UUID)) == 0 {
		t.Fatal("old writer did not flush a disk cache entry")
	}
	if err = os.WriteFile(filepath.Join(root, "slice-id"), []byte(strconv.FormatUint(id, 10)), 0600); err != nil {
		t.Fatal(err)
	}
}

func countCacheFiles(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			if info.Size() > 0 {
				count++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}
