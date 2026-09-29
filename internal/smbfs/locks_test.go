// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"context"
	"errors"
	"math"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func TestNativeLocksCancellationAndClose(t *testing.T) {
	f := newFixture(t)
	s := f.s
	a := openFile(t, s, "locked")
	b := openFile(t, s, "locked")
	exclusive := vfs.ByteRangeLock{Offset: 10, Length: 10, Type: vfs.ByteRangeLockExclusive, FailImmediately: true}
	if e := s.Lock(a, []vfs.ByteRangeLock{exclusive}); e != nil {
		t.Fatal(e)
	}
	// Query native metadata independently: adapter locks are not a local no-op.
	typ := uint32(syscall.F_WRLCK)
	start, end := uint64(10), uint64(19)
	var pid uint32
	if er := f.m.Getlk(s.ctx, s.handles[a].file.Inode(), uint64(b), &typ, &start, &end, &pid); er != 0 || typ != syscall.F_WRLCK {
		t.Fatalf("native conflict=%d %v", typ, er)
	}
	requireErr(t, s.Lock(b, []vfs.ByteRangeLock{exclusive}), syscall.EAGAIN)
	_, e := s.Write(b, []byte("x"), 10, 0)
	requireErr(t, e, syscall.EAGAIN)
	adjacent := exclusive
	adjacent.Offset = 20
	if e = s.Lock(b, []vfs.ByteRangeLock{adjacent}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Write(b, []byte("x"), 20, 0); e != nil {
		t.Fatal(e)
	}
	waiting := exclusive
	waiting.FailImmediately = false
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- s.LockContext(ctx, b, []vfs.ByteRangeLock{waiting}) }()
	cancel()
	select {
	case e = <-finished:
		requireErr(t, e, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("lock cancellation stuck")
	}
	go func() { finished <- s.Lock(b, []vfs.ByteRangeLock{waiting}) }()
	if e = s.Close(b); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-finished:
		requireErr(t, e, syscall.EBADF)
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel waiting lock")
	}
	if e = s.Close(a); e != nil {
		t.Fatal(e)
	}
	c := openFile(t, s, "locked")
	if e = s.Lock(c, []vfs.ByteRangeLock{exclusive, adjacent}); e != nil {
		t.Fatalf("close leaked native locks: %v", e)
	}
	if e = s.Shutdown(); e != nil {
		t.Fatal(e)
	}
	_, e = s.Open("locked", 0, 0)
	requireErr(t, e, syscall.EBADF)
}
func TestNativeSharedLocksAndBatchRollback(t *testing.T) {
	s := newFixture(t).s
	a := openFile(t, s, "locked")
	b := openFile(t, s, "locked")
	shared := vfs.ByteRangeLock{Offset: 0, Length: 10, Type: vfs.ByteRangeLockShared, FailImmediately: true}
	if e := s.Lock(a, []vfs.ByteRangeLock{shared}); e != nil {
		t.Fatal(e)
	}
	if e := s.Lock(b, []vfs.ByteRangeLock{shared}); e != nil {
		t.Fatal(e)
	}
	exclusive := shared
	exclusive.Type = vfs.ByteRangeLockExclusive
	requireErr(t, s.Lock(b, []vfs.ByteRangeLock{exclusive}), syscall.EAGAIN)
	unlock := shared
	unlock.Type = vfs.ByteRangeLockUnlock
	if e := s.Lock(b, []vfs.ByteRangeLock{unlock}); e != nil {
		t.Fatal(e)
	}
	nonconflict := exclusive
	nonconflict.Offset = 100
	requireErr(t, s.Lock(b, []vfs.ByteRangeLock{nonconflict, exclusive}), syscall.EAGAIN)
	if e := s.Lock(a, []vfs.ByteRangeLock{nonconflict}); e != nil {
		t.Fatalf("failed batch leaked range: %v", e)
	}
	requireErr(t, s.Lock(b, []vfs.ByteRangeLock{unlock}), syscall.ENOLCK)
	for _, l := range []vfs.ByteRangeLock{{Length: 0, Type: vfs.ByteRangeLockShared}, {Offset: math.MaxUint64, Length: 2, Type: vfs.ByteRangeLockShared}, {Length: 1, Type: 99}} {
		requireErr(t, s.Lock(a, []vfs.ByteRangeLock{l}), syscall.EINVAL)
	}
	// Unlocking one overlapping own range must retain the other native range.
	overlap := shared
	overlap.Offset = 5
	if e := s.Lock(a, []vfs.ByteRangeLock{overlap}); e != nil {
		t.Fatal(e)
	}
	if e := s.Lock(a, []vfs.ByteRangeLock{unlock}); e != nil {
		t.Fatal(e)
	}
	exclusive.Offset = 10
	exclusive.Length = 1
	if e := s.Lock(b, []vfs.ByteRangeLock{exclusive}); !errors.Is(e, syscall.EAGAIN) {
		t.Fatalf("overlap lost: %v", e)
	}
}
