// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryWithRetainedCache(t *testing.T) {
	f := newFixture(t, false)
	f.cacheSize = "8 MB"
	f.interval = "1h"
	d := f.start()
	s, disconnect := f.share()
	writeFile(t, s, "old.txt", []byte("OLDOLD"))
	verifyFiles(t, s, map[string][]byte{"old.txt": []byte("OLDOLD")})
	disconnect()

	cacheRoot := filepath.Join(f.root, "cache")
	// The disk cache is written in the background; wait for it before
	// losing the local state.
	waitCached(t, cacheRoot)
	sigkill(t, d)
	if err := os.RemoveAll(filepath.Join(f.root, "state")); err != nil {
		t.Fatal(err)
	}

	unrelated := map[string]string{
		"keep.txt":              "unrelated root file",
		"other-volume/keep.txt": "unrelated directory",
	}
	for name, data := range unrelated {
		path := filepath.Join(cacheRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	d = f.start()
	s, disconnect = f.share()
	entries, err := s.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "old.txt" {
			t.Fatal("recovery did not select the earlier empty backup")
		}
	}
	writeFile(t, s, "new.txt", []byte("NEWNEW"))
	verifyFiles(t, s, map[string][]byte{"new.txt": []byte("NEWNEW")})
	disconnect()
	d.stop()
	cache, err := os.OpenRoot(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	}()
	for name, want := range unrelated {
		got, err := cache.ReadFile(name)
		if err != nil || string(got) != want {
			t.Fatalf("unrelated cache root entry %s changed: %q, %v", name, got, err)
		}
	}
}

// waitCached waits until the disk cache under root holds a data block.
func waitCached(t *testing.T, root string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		cached := false
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.Type().IsRegular() && strings.Contains(path, string(os.PathSeparator)+"raw"+string(os.PathSeparator)) {
				cached = true
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if cached {
			return
		}
	}
	t.Fatal("file never reached the disk cache")
}
