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
	s, closeShare := f.share()
	writeFile(t, s, "old.txt", []byte("OLDOLD"))
	verifyFiles(t, s, map[string][]byte{"old.txt": []byte("OLDOLD")})
	closeShare()

	// Wait for the asynchronous disk cache write before losing local state.
	cacheRoot := filepath.Join(f.root, "cache")
	deadline := time.Now().Add(5 * time.Second)
	cached := false
	for !cached && time.Now().Before(deadline) {
		err := filepath.WalkDir(cacheRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() && strings.Contains(path, string(os.PathSeparator)+"raw"+string(os.PathSeparator)) {
				cached = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !cached {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !cached {
		t.Fatal("old file never reached the disk cache")
	}
	if err := d.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-d.done; err == nil {
		t.Fatal("killed daemon exited successfully")
	}
	d.stopped = true
	d.closeLogs()
	if err := os.RemoveAll(filepath.Join(f.root, "state")); err != nil {
		t.Fatal(err)
	}

	unrelated := map[string]string{
		"keep.txt":              "unrelated root file",
		"other-volume/keep.txt": "unrelated directory",
	}
	for name, data := range unrelated {
		path := filepath.Join(cacheRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}

	d = f.start()
	s, closeShare = f.share()
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
	closeShare()
	d.stop()
	for name, want := range unrelated {
		got, err := os.ReadFile(filepath.Join(cacheRoot, name))
		if err != nil || string(got) != want {
			t.Fatalf("unrelated cache root entry %s changed: %q, %v", name, got, err)
		}
	}
}
