// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"os"
	"path/filepath"
	"testing"
)

const cacheTestUUID = "6f4f1a3b-5370-4383-b974-5d1bd26191b4"

func TestWipeVolumeCache(t *testing.T) {
	root, state, outside := t.TempDir(), t.TempDir(), t.TempDir()
	volume := filepath.Join(root, cacheTestUUID)
	files := map[string]string{
		filepath.Join(volume, "raw", "old"):         "old cache",
		filepath.Join(root, "keep"):                 "root file",
		filepath.Join(root, "other-volume", "keep"): "other cache",
		filepath.Join(state, "keep"):                "state file",
		filepath.Join(outside, "keep"):              "symlink target",
	}
	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(volume, "link")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := WipeVolumeCache(root, cacheTestUUID, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(volume); !os.IsNotExist(err) {
		t.Fatalf("volume cache remains: %v", err)
	}
	delete(files, filepath.Join(volume, "raw", "old"))
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("unrelated file changed: %s: %q, %v", path, got, err)
		}
	}
	if err := WipeVolumeCache(filepath.Join(root, "missing"), cacheTestUUID, state); err != nil {
		t.Fatal(err)
	}
}

func TestWipeVolumeCacheRejectsUnsafePaths(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	for _, id := range []string{"", ".", "..", "../state", "/", "not-a-uuid"} {
		if err := WipeVolumeCache(root, id, state); err == nil {
			t.Fatalf("accepted unsafe volume UUID %q", id)
		}
	}
	if err := WipeVolumeCache("relative", cacheTestUUID, state); err == nil {
		t.Fatal("accepted relative cache root")
	}
	if err := WipeVolumeCache(root, cacheTestUUID, "relative"); err == nil {
		t.Fatal("accepted relative state directory")
	}
	volume := filepath.Join(root, cacheTestUUID)
	state = filepath.Join(volume, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "cache-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, cache := range []string{root, alias} {
		for _, stateDir := range []string{volume, state, root} {
			if err := WipeVolumeCache(cache, cacheTestUUID, stateDir); err == nil {
				t.Fatalf("accepted cache %s overlapping state %s", cache, stateDir)
			}
		}
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state directory was touched: %v", err)
	}
}

func TestWipeVolumeCacheErrors(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WipeVolumeCache(file, cacheTestUUID, state); err == nil {
		t.Fatal("accepted a file as the cache root")
	}
	if err := WipeVolumeCache(root, cacheTestUUID, filepath.Join(state, "missing")); err == nil {
		t.Fatal("ignored state directory lookup error")
	}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if err := WipeVolumeCache(loop, cacheTestUUID, state); err == nil {
		t.Fatal("ignored cache root lookup error")
	}
}
