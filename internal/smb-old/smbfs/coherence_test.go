// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"errors"
	"io"
	"syscall"
	"testing"
)

func TestNativeCrossHandleReadFailure(t *testing.T) {
	f := newFixture(t)
	writer := openFile(t, f.s, "file")
	reader, e := f.s.Open("file", syscall.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	f.store.fail.Store(true)
	if _, e = f.s.Write(writer, []byte("uncommitted"), 0, 0); e != nil {
		t.Fatal(e)
	}
	_, e = f.s.Read(reader, make([]byte, 16), 0, 0)
	if !errors.Is(e, syscall.EIO) {
		t.Fatalf("read acknowledged/obscured failed other-handle flush: %v", e)
	}
}

func TestNativeCrossHandleReadCoherence(t *testing.T) {
	s := newFixture(t).s
	writer := openFile(t, s, "file")
	reader, e := s.Open("file", syscall.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	original := []byte("first content")
	if _, e = s.Write(writer, original, 0, 0); e != nil {
		t.Fatal(e)
	}
	// Flush before reading isolates a stale open-time size from upload visibility.
	if e = s.Flush(writer); e != nil {
		t.Fatal(e)
	}
	got := make([]byte, 64)
	n, e := s.Read(reader, got, 0, 0)
	if e != nil || !bytes.Equal(got[:n], original) {
		t.Fatalf("cross-handle read after flushed write = %q, %v; want %q", got[:n], e, original)
	}
	if e = s.Truncate(writer, 3); e != nil {
		t.Fatal(e)
	}
	n, e = s.Read(reader, got, 0, 0)
	if e != nil || string(got[:n]) != "fir" {
		t.Fatalf("read after shrink = %q, %v", got[:n], e)
	}
	if e = s.Truncate(writer, 0); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Read(reader, got, 0, 0); e != io.EOF {
		t.Fatalf("read after empty = %v", e)
	}
	replacement := []byte("new and longer content")
	if _, e = s.Write(writer, replacement, 0, 0); e != nil {
		t.Fatal(e)
	}
	// Reads must also observe the native inode's buffered writer, not just their
	// own handle's writer. No adapter attribute cache or reopen-by-path shortcut.
	n, e = s.Read(reader, got, 0, 0)
	if e != nil || !bytes.Equal(got[:n], replacement) {
		t.Fatalf("read after regrow = %q, %v", got[:n], e)
	}
}
