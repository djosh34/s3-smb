// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The rules in docs/vendored.md that keep `go install module@version` working.
func TestPackaging(t *testing.T) {
	root := os.DirFS(".")
	for _, name := range []string{"LICENSE", "NOTICE", "internal/smb/auth/LICENSE", "internal/smb/auth/Attributions.txt"} {
		if b, err := fs.ReadFile(root, name); err != nil || len(b) < 100 {
			t.Errorf("missing licence file %s: %v", name, err)
		}
	}
	mod, err := fs.ReadFile(root, "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).Match(mod) {
		t.Error("go.mod has a replace directive")
	}
	upstream := []string{"github.com/macos-fuse-t/go-smb2"}
	err = fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == ".git" {
			return fs.SkipDir
		}
		if strings.HasPrefix(path, "internal/") && (d.Name() == "go.mod" || d.Name() == "go.work" || d.Name() == "vendor") {
			t.Errorf("%s must not exist under internal", path)
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		imports, err := importPaths(root, path)
		for _, name := range imports {
			for _, prefix := range upstream {
				if strings.HasPrefix(name, prefix) {
					t.Errorf("%s imports upstream path %s", path, name)
				}
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func importPaths(root fs.FS, path string) ([]string, error) {
	src, err := fs.ReadFile(root, path)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(f.Imports))
	for i, imp := range f.Imports {
		if paths[i], err = strconv.Unquote(imp.Path.Value); err != nil {
			return nil, err
		}
	}
	return paths, nil
}
