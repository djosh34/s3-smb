// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The rules in docs/vendored.md that keep `go install module@version` working.
func TestPackaging(t *testing.T) {
	for _, name := range []string{"LICENSE", "NOTICE", "internal/juicefs/LICENSE", "internal/smb-old/smb2/LICENSE", "internal/smb-old/smb2/Attributions.txt", "internal/thirdparty/mpb/UNLICENSE", "internal/thirdparty/xorm/LICENSE"} {
		if b, err := os.ReadFile(name); err != nil || len(b) < 100 {
			t.Errorf("missing licence file %s: %v", name, err)
		}
	}
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).Match(mod) {
		t.Error("go.mod has a replace directive")
	}
	upstream := []string{"github.com/juicedata/juicefs", "github.com/macos-fuse-t/go-smb2", "xorm.io/xorm", "github.com/vbauerster/mpb/v7", "github.com/urfave/cli/v2", "github.com/hashicorp/golang-lru/v2", "github.com/hanwen/go-fuse"}
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == ".git" {
			return filepath.SkipDir
		}
		if strings.HasPrefix(path, "internal/") && (d.Name() == "go.mod" || d.Name() == "go.work" || d.Name() == "vendor") {
			t.Errorf("%s must not exist under internal", path)
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			name, _ := strconv.Unquote(imp.Path.Value)
			for _, prefix := range upstream {
				if strings.HasPrefix(name, prefix) {
					t.Errorf("%s imports upstream path %s", path, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
