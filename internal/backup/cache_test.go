// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/djosh34/s3-smb/internal/storage"
)

const cacheTestUUID = "6f4f1a3b-5370-4383-b974-5d1bd26191b4"

func writeFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func checkFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for path, want := range files {
		if got, err := os.ReadFile(filepath.Clean(path)); err != nil || string(got) != want {
			t.Fatalf("file changed: %s: %q, %v", path, got, err)
		}
	}
}

func TestWipeVolumeCacheRemovesOnlyTheVolume(t *testing.T) {
	root, state, outside := t.TempDir(), t.TempDir(), t.TempDir()
	volume := filepath.Join(root, cacheTestUUID)
	writeFiles(t, map[string]string{filepath.Join(volume, "raw", "old"): "old cache"})
	keep := map[string]string{
		filepath.Join(root, "keep"):                 "root file",
		filepath.Join(root, "other-volume", "keep"): "other cache",
		filepath.Join(state, "keep"):                "state file",
		filepath.Join(outside, "keep"):              "symlink target",
	}
	writeFiles(t, keep)
	if err := os.Symlink(outside, filepath.Join(volume, "link")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := WipeVolumeCache(root, cacheTestUUID, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(volume); !os.IsNotExist(err) {
		t.Fatalf("volume cache remains: %v", err)
	}
	checkFiles(t, keep)
	if err := WipeVolumeCache(filepath.Join(root, "missing"), cacheTestUUID, state); err != nil {
		t.Fatal(err)
	}
	// A symlink in place of the volume directory is unlinked, not followed.
	if err := os.Symlink(state, volume); err != nil {
		t.Fatal(err)
	}
	if err := WipeVolumeCache(root, cacheTestUUID, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(volume); !os.IsNotExist(err) {
		t.Fatalf("volume symlink remains: %v", err)
	}
	checkFiles(t, keep)
}

func TestWipeVolumeCacheRefusesStateOverlap(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, cacheTestUUID)
	state := filepath.Join(volume, "state")
	files := map[string]string{filepath.Join(state, "keep"): "state file"}
	writeFiles(t, files)
	rootAlias := filepath.Join(t.TempDir(), "cache-alias")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatal(err)
	}
	for _, cache := range []string{root, rootAlias} {
		for _, stateDir := range []string{volume, state} {
			if err := WipeVolumeCache(cache, cacheTestUUID, stateDir); err == nil {
				t.Fatalf("accepted cache %s overlapping state %s", cache, stateDir)
			}
		}
	}
	// The volume cache inside the state directory.
	stateAlias := filepath.Join(t.TempDir(), "state-alias")
	if err := os.Symlink(volume, stateAlias); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(volume, cacheTestUUID)
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, stateDir := range []string{volume, stateAlias} {
		if err := WipeVolumeCache(volume, cacheTestUUID, stateDir); err == nil {
			t.Fatalf("accepted a cache inside state directory %s", stateDir)
		}
	}
	if _, err := os.Stat(inner); err != nil {
		t.Fatalf("volume cache was touched: %v", err)
	}
	checkFiles(t, files)
}

func TestWipeVolumeCacheCaseInsensitiveState(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	state := filepath.Join(root, cacheTestUUID, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "Cache", cacheTestUUID, "state")
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		t.Skip("needs a case-insensitive filesystem")
	} else if err != nil {
		t.Fatal(err)
	}
	if err := WipeVolumeCache(root, cacheTestUUID, alias); err == nil {
		t.Fatal("accepted a second spelling of the state directory")
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state directory was touched: %v", err)
	}
}

func TestWipeVolumeCacheRejectsInvalidInput(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	for _, id := range []string{"", "..", "not-a-uuid"} {
		if err := WipeVolumeCache(root, id, state); err == nil {
			t.Fatalf("accepted volume UUID %q", id)
		}
	}
	if err := WipeVolumeCache("relative", cacheTestUUID, state); err == nil {
		t.Fatal("accepted a relative cache root")
	}
	if err := WipeVolumeCache(root, cacheTestUUID, "relative"); err == nil {
		t.Fatal("accepted a relative state directory")
	}
	file := filepath.Join(root, "not-a-directory")
	writeFiles(t, map[string]string{file: "keep"})
	if err := WipeVolumeCache(file, cacheTestUUID, state); err == nil {
		t.Fatal("accepted a file as the cache root")
	}
	if err := WipeVolumeCache(root, cacheTestUUID, filepath.Join(state, "missing")); err == nil {
		t.Fatal("ignored a state directory lookup error")
	}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if err := WipeVolumeCache(loop, cacheTestUUID, state); err == nil {
		t.Fatal("ignored a cache root lookup error")
	}
}

// JuiceFS cleans the configured cache path before opening it, rather than
// following a symlink before "..". The wipe must target the same directory.
func TestWipeVolumeCacheMatchesJuiceFSPath(t *testing.T) {
	format, err := storage.NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	for name, physicalRootExists := range map[string]bool{"missing physical root": false, "existing physical root": true} {
		t.Run(name, func(t *testing.T) {
			base, state := t.TempDir(), t.TempDir()
			target := filepath.Join(base, "x", "y")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(base, "a")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			root := alias + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "b"
			capacity := uint64(8 << 20)
			conf, err := storage.CacheConfig(format, root, &capacity)
			if err != nil {
				t.Fatal(err)
			}
			writeFiles(t, map[string]string{filepath.Join(conf.CacheDir, "old"): "old cache"})
			unrelated := map[string]string{}
			if physicalRootExists {
				unrelated[filepath.Join(base, "x", "b", format.UUID, "keep")] = "unrelated file"
				writeFiles(t, unrelated)
			}
			if err := WipeVolumeCache(root, format.UUID, conf.CacheDir); err == nil {
				t.Fatal("accepted a cache path that names the state directory")
			}
			if err := WipeVolumeCache(root, format.UUID, state); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(conf.CacheDir); !os.IsNotExist(err) {
				t.Fatalf("JuiceFS cache remains at %s: %v", conf.CacheDir, err)
			}
			checkFiles(t, unrelated)
		})
	}
}
