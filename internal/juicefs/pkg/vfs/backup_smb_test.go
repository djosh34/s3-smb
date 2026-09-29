// SPDX-License-Identifier: AGPL-3.0-only
package vfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

type dumpStub struct {
	meta.Meta
	dump func(io.Writer) error
}

func (m dumpStub) DumpMeta(w io.Writer, _ meta.Ino, _ int, _, _, _ bool) error { return m.dump(w) }

type fullWriter struct{ remaining int }

func (w *fullWriter) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, syscall.ENOSPC
	}
	w.remaining -= len(b)
	return len(b), nil
}
func TestBackupGzipFinalizationError(t *testing.T) {
	dumped := false
	m := dumpStub{dump: func(w io.Writer) error { _, e := w.Write([]byte("x")); dumped = e == nil; return e }}
	err := WriteBackup(&fullWriter{remaining: 10}, m) // gzip header fits, final block/trailer does not
	if !dumped {
		t.Fatal("fault occurred before successful dump; not finalization injection")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("gzip close failure ignored: %v", err)
	}
	var good bytes.Buffer
	if err = WriteBackup(&good, m); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&good)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(gz)
	if err != nil || string(b) != "x" {
		t.Fatalf("native gzip corrupt %q %v", b, err)
	}
}

type uploadProbe struct {
	object.ObjectStorage
	puts   int
	data   []byte
	exists bool
}

func (p *uploadProbe) Head(context.Context, string) (object.Object, error) {
	if p.exists {
		return nil, nil
	}
	return nil, os.ErrNotExist
}
func (p *uploadProbe) PutIfAbsent(_ context.Context, _ string, r io.Reader) error {
	p.puts++
	var e error
	p.data, e = io.ReadAll(r)
	p.exists = true
	return e
}
func (p *uploadProbe) Get(context.Context, string, int64, int64, ...object.AttrGetter) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(p.data)), nil
}
func TestBackupStagingFailureNeverUploads(t *testing.T) {
	m := dumpStub{dump: func(w io.Writer) error { _, _ = w.Write([]byte("partial")); return syscall.ENOSPC }}
	p := &uploadProbe{}
	if _, err := BackupTo(context.Background(), m, p, t.TempDir(), "meta/dump-2026-01-01-000000.json.gz"); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	if p.puts != 0 {
		t.Fatal("partial staging uploaded")
	}
	path := filepath.Join(t.TempDir(), "not-directory")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupTo(context.Background(), m, p, path, "key"); err == nil {
		t.Fatal("invalid staging path succeeded")
	}
	if p.puts != 0 {
		t.Fatal("staging failure uploaded")
	}
}
func TestNativeBackupPrivateStagingAndReadback(t *testing.T) {
	dir := t.TempDir()
	p := &uploadProbe{}
	m := dumpStub{dump: func(w io.Writer) error {
		files, e := os.ReadDir(dir)
		if e != nil {
			return e
		}
		if len(files) != 1 {
			t.Fatalf("stage files=%d", len(files))
		}
		st, e := files[0].Info()
		if e != nil {
			return e
		}
		if st.Mode().Perm() != 0600 {
			t.Fatalf("stage mode %o", st.Mode().Perm())
		}
		_, e = w.Write([]byte("{}"))
		return e
	}}
	if _, err := BackupTo(context.Background(), m, p, dir, "meta/dump-2026-01-01-000000.json.gz"); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatal("stage leaked")
	}
	if _, err := BackupTo(context.Background(), m, p, dir, "meta/dump-2026-01-01-000000.json.gz"); err == nil {
		t.Fatal("existing name overwritten")
	}
	if p.puts != 1 {
		t.Fatalf("puts=%d", p.puts)
	}
}
