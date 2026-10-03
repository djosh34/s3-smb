// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

func TestNativeXattrBoundAndMissingAttribute(t *testing.T) {
	s := newFixture(t).s
	h := openFile(t, s, "resource-fork")
	key := "com.apple.ResourceFork"
	_, e := s.Getxattr(h, key, nil)
	requireErr(t, e, meta.ENOATTR) // ENODATA on Linux, ENOATTR on Darwin.
	value := bytes.Repeat([]byte{0xa5}, vfs.MaxXattrSize)
	if e = s.Setxattr(h, key, value); e != nil {
		t.Fatalf("exact native bound: %v", e)
	}
	n, e := s.Getxattr(h, key, nil)
	if e != nil || n != len(value) {
		t.Fatalf("size probe = %d, %v", n, e)
	}
	requireErr(t, s.Setxattr(h, key, make([]byte, vfs.MaxXattrSize+1)), syscall.E2BIG)
	got := make([]byte, len(value))
	n, e = s.Getxattr(h, key, got)
	if e != nil || !bytes.Equal(got[:n], value) {
		t.Fatalf("failed oversized set changed existing xattr: %d, %v", n, e)
	}
	if e = s.Setxattr(h, key, nil); e != nil {
		t.Fatalf("empty value: %v", e)
	}
	n, e = s.Getxattr(h, key, nil)
	if e != nil || n != 0 {
		t.Fatalf("empty is not missing: %d, %v", n, e)
	}
	if e = s.Removexattr(h, key); e != nil {
		t.Fatal(e)
	}
	_, e = s.Getxattr(h, key, nil)
	requireErr(t, e, meta.ENOATTR)
}
