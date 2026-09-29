// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func TestNativeReadOnlyLocksAndClose(t *testing.T) {
	writable := newFixture(t)
	h := openFile(t, writable.s, "file")
	if _, e := writable.s.Write(h, []byte("readonly"), 0, 0); e != nil {
		t.Fatal(e)
	}
	if e := writable.s.Flush(h); e != nil {
		t.Fatal(e)
	}
	var dump bytes.Buffer
	if e := writable.m.DumpMeta(&dump, meta.RootInode, 1, true, true, false); e != nil {
		t.Fatal(e)
	}
	ro := fixtureWithStore(t, writable.store, dump.Bytes(), true)
	a, e := ro.s.Open("file", syscall.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	b, e := ro.s.Open("file", syscall.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	shared := vfs.ByteRangeLock{Offset: 0, Length: 8, Type: vfs.ByteRangeLockShared, FailImmediately: true}
	if e = ro.s.Lock(a, []vfs.ByteRangeLock{shared}); e != nil {
		t.Fatal(e)
	}
	if e = ro.s.Lock(b, []vfs.ByteRangeLock{shared}); e != nil {
		t.Fatal(e)
	}
	exclusive := shared
	exclusive.Type = vfs.ByteRangeLockExclusive
	requireErr(t, ro.s.Lock(b, []vfs.ByteRangeLock{exclusive}), syscall.EAGAIN)
	unlock := shared
	unlock.Type = vfs.ByteRangeLockUnlock
	if e = ro.s.Lock(a, []vfs.ByteRangeLock{unlock}); e != nil {
		t.Fatal(e)
	}
	if e = ro.s.Lock(b, []vfs.ByteRangeLock{exclusive}); e != nil {
		t.Fatal(e)
	}
	// Native mutation must remain forbidden independently of the adapter guard.
	_, er := ro.native.Create(ro.s.ctx, "/forbidden", 0600, 0)
	requireErr(t, errno(er), syscall.EROFS)
	if e = ro.s.Flush(a); e != nil {
		t.Fatal(e)
	}
	if e = ro.s.Close(a); e != nil {
		t.Fatal(e)
	}
	if e = ro.s.Close(b); e != nil {
		t.Fatal(e)
	}
	directory, e := ro.s.OpenDir("")
	if e != nil {
		t.Fatal(e)
	}
	if e = ro.s.Flush(directory); e != nil {
		t.Fatal(e)
	}
	if e = ro.s.Close(directory); e != nil {
		t.Fatal(e)
	}
}
