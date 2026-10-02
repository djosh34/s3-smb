// SPDX-License-Identifier: AGPL-3.0-only
package packaging

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/compress"
	_ "github.com/mattn/go-sqlite3"
)

func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate source tree")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func TestSourceDistribution(t *testing.T) {
	dir := root(t)
	for _, name := range []string{"LICENSE", "NOTICE", "internal/juicefs/LICENSE", "internal/smb2/LICENSE", "internal/smb2/Attributions.txt", "internal/thirdparty/cli/LICENSE", "internal/thirdparty/mpb/UNLICENSE", "internal/thirdparty/xorm/LICENSE"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(b) < 100 {
			t.Errorf("missing license/notice %s: %v", name, err)
		}
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mod), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "replace ") || strings.TrimSpace(line) == "replace (" {
			t.Error("effective replacement in root go.mod")
		}
	}
	if !bytes.HasPrefix(mod, []byte("module github.com/djosh34/s3-smb\n")) {
		t.Error("wrong root module")
	}
	b, err := os.ReadFile(filepath.Join(dir, "docs/source-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Snapshots map[string]struct{ Commit string }
		Files     []struct{ Destination string }
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{
		"github.com/juicedata/juicefs":    "0b90c7db5a929ae6adc5faad948d108efd2c99f9",
		"github.com/macos-fuse-t/go-smb2": "277a9300411249a881a05f7a910f5a83ae3395f2",
	}
	for name, pin := range pins {
		if manifest.Snapshots[name].Commit != pin {
			t.Errorf("wrong source pin for %s", name)
		}
	}
	if len(manifest.Files) < 100 {
		t.Fatal("source manifest unexpectedly empty")
	}
	err = filepath.WalkDir(filepath.Join(dir, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == "go.mod" || d.Name() == "go.work" || d.Name() == "vendor" {
			t.Errorf("forbidden nested module/workspace/vendor: %s", path)
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
			for _, prefix := range []string{"github.com/juicedata/juicefs", "github.com/macos-fuse-t/go-smb2", "github.com/hanwen/go-fuse", "github.com/winfsp/cgofuse", "github.com/ceph/go-ceph", "github.com/juicedata/gogfapi", "github.com/apple/foundationdb", "xorm.io/xorm", "github.com/urfave/cli/v2", "github.com/vbauerster/mpb/v7", "github.com/hashicorp/golang-lru/v2"} {
				if strings.HasPrefix(name, prefix) {
					t.Errorf("unbundled/unsupported native import %s in %s", name, path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Exercise the actual portable C sources shipped in the pinned Go dependencies;
// this is a packaging smoke test, not an application durability or S3 test.
func TestBundledPortableCGo(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "smoke.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE smoke(value TEXT); INSERT INTO smoke VALUES ('bundled-cgo')"); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow("SELECT value FROM smoke").Scan(&value); err != nil || value != "bundled-cgo" {
		t.Fatalf("SQLite smoke %q: %v", value, err)
	}
	data := bytes.Repeat([]byte("native compression smoke\x00"), 200)
	for _, name := range []string{"zstd", "lz4", "none"} {
		t.Run(name, func(t *testing.T) {
			c := compress.NewCompressor(name)
			if c == nil {
				t.Fatalf("missing compressor %s", name)
			}
			encoded := make([]byte, c.CompressBound(len(data)))
			n, err := c.Compress(encoded, data)
			if err != nil {
				t.Fatal(err)
			}
			decoded := make([]byte, len(data))
			n, err = c.Decompress(decoded, encoded[:n])
			if err != nil || !bytes.Equal(decoded[:n], data) {
				t.Fatalf("compression roundtrip: %v", err)
			}
		})
	}
}
