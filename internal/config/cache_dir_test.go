// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheDirectoryRejectsListsAndGlobs(t *testing.T) {
	paths := []string{
		"/tmp/a:/tmp/b",
		"/tmp/a,/tmp/b",
		"./cache:other",
		"./cache,other",
		"./cache-*",
		"./cache-?",
		"./cache-[ab]",
		`./cache\dir`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, size := range []string{"0", "8 MB"} {
				_, err := loadText(t, validYAML+fmt.Sprintf("storage: {cache_dir: %q, cache_size: %q}\n", path, size))
				if err == nil || !strings.Contains(err.Error(), "storage.cache_dir") {
					t.Fatalf("cache directory %q with size %s: %v", path, size, err)
				}
			}
		})
	}
}

func TestCacheDirectoryAcceptsOnePath(t *testing.T) {
	for _, path := range []string{"./cache", "./cache with spaces", filepath.Join(t.TempDir(), "cache")} {
		c, err := loadText(t, validYAML+fmt.Sprintf("storage: {cache_dir: %q}\n", path))
		if err != nil {
			t.Fatal(err)
		}
		want := path
		if !filepath.IsAbs(want) {
			want = filepath.Join(filepath.Dir(c.path), want)
		}
		if c.Storage.CacheDir != want {
			t.Fatalf("cache directory: got %q, want %q", c.Storage.CacheDir, want)
		}
	}
}

func TestCacheDirectoryRejectsInheritedPatterns(t *testing.T) {
	for _, character := range []string{":", ",", "*", "?", "[", `\`} {
		t.Run(character, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "parent"+character)
			t.Setenv("XDG_CACHE_HOME", dir)
			if _, err := loadText(t, validYAML); err == nil || !strings.Contains(err.Error(), "storage.cache_dir") {
				t.Fatalf("accepted default cache directory %q: %v", dir, err)
			}
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(validYAML+"storage: {cache_dir: ./cache}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "storage.cache_dir") {
				t.Fatalf("accepted resolved cache directory under %q: %v", dir, err)
			}
		})
	}
}
