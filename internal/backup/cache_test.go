// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/djosh34/s3-smb/internal/storage"
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
		for _, stateDir := range []string{volume, state} {
			if err := WipeVolumeCache(cache, cacheTestUUID, stateDir); err == nil {
				t.Fatalf("accepted cache %s overlapping state %s", cache, stateDir)
			}
		}
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state directory was touched: %v", err)
	}
}

func TestWipeVolumeCacheInsideState(t *testing.T) {
	state := t.TempDir()
	volume := filepath.Join(state, cacheTestUUID)
	if err := os.Mkdir(volume, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(state, "keep")
	if err := os.WriteFile(keep, []byte("state file"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "state-alias")
	if err := os.Symlink(state, alias); err != nil {
		t.Fatal(err)
	}
	for _, stateDir := range []string{state, alias} {
		if err := WipeVolumeCache(state, cacheTestUUID, stateDir); err == nil {
			t.Fatalf("accepted a cache inside state directory %s", stateDir)
		}
	}
	if _, err := os.Stat(volume); err != nil {
		t.Fatalf("volume cache was touched: %v", err)
	}
	got, err := os.ReadFile(keep)
	if err != nil || string(got) != "state file" {
		t.Fatalf("state file changed: %q, %v", got, err)
	}
}

func TestDirectoryContainsAliases(t *testing.T) {
	state := t.TempDir()
	alias := filepath.Join(t.TempDir(), "state-alias")
	if err := os.Symlink(state, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(state, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{alias, filepath.Join(alias, "child")} {
		contains, err := directoryContains(info, path)
		if err != nil || !contains {
			t.Fatalf("directory alias %s not recognized: %v", path, err)
		}
	}
	if _, err := directoryContains(info, filepath.Join(state, "missing")); err == nil {
		t.Fatal("ignored directory lookup error")
	}
}

func TestWipeVolumeCacheCaseInsensitiveState(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	state := filepath.Join(root, cacheTestUUID, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
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

func TestWipeVolumeCacheUnlinksVolumeSymlink(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	keep := filepath.Join(state, "keep")
	if err := os.WriteFile(keep, []byte("state file"), 0600); err != nil {
		t.Fatal(err)
	}
	volume := filepath.Join(root, cacheTestUUID)
	if err := os.Symlink(state, volume); err != nil {
		t.Fatal(err)
	}
	if err := WipeVolumeCache(root, cacheTestUUID, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(volume); !os.IsNotExist(err) {
		t.Fatalf("volume symlink remains: %v", err)
	}
	got, err := os.ReadFile(keep)
	if err != nil || string(got) != "state file" {
		t.Fatalf("symlink target was touched: %q, %v", got, err)
	}
}

func TestWipeVolumeCacheMatchesJuiceFSPath(t *testing.T) {
	format, err := storage.NewFormat("test", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	for name, physicalRootExists := range map[string]bool{"missing physical root": false, "existing physical root": true} {
		t.Run(name, func(t *testing.T) {
			base, state := t.TempDir(), t.TempDir()
			target := filepath.Join(base, "x", "y")
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(base, "a")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			// Keep .. in the configured string. JuiceFS cleans it before opening
			// the cache, rather than following the symlink first.
			root := alias + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "b"
			capacity := int64(8 << 20)
			conf, err := storage.CacheConfig(format, root, &capacity)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(conf.CacheDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(conf.CacheDir, "old"), []byte("old cache"), 0600); err != nil {
				t.Fatal(err)
			}
			unrelated := filepath.Join(base, "x", "b", format.UUID, "keep")
			if physicalRootExists {
				if err := os.MkdirAll(filepath.Dir(unrelated), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unrelated, []byte("unrelated file"), 0600); err != nil {
					t.Fatal(err)
				}
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
			if physicalRootExists {
				got, err := os.ReadFile(unrelated)
				if err != nil || string(got) != "unrelated file" {
					t.Fatalf("unrelated file changed: %q, %v", got, err)
				}
			}
		})
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
