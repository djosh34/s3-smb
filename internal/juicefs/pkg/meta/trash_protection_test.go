// SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"sync/atomic"
	"syscall"
	"testing"
)

// Stage-2 security review's executed reproducer, retained as regression evidence.
// This is the native emptyDir -> BatchUnlink -> SQL batch-retirement path, not
// just a call to the maintenance callback or single-entry unlink helper.
func TestSecurityReviewExpiredTrashBatchRetirement(t *testing.T) {
	var expired atomic.Bool
	var calls atomic.Int32
	m := protectedDB(t, func() error {
		calls.Add(1)
		if expired.Load() {
			return syscall.EROFS
		}
		return nil
	})
	parent := TrashInode + 1
	leaf := Ino(42)
	if _, err := m.db.Insert(
		&node{Inode: parent, Type: TypeDirectory, Mode: 0555, Nlink: 2, Parent: TrashInode},
		&node{Inode: leaf, Type: TypeSymlink, Mode: 0777, Nlink: 1, Parent: parent},
		&edge{Parent: parent, Name: []byte("expired-link"), Inode: leaf, Type: TypeSymlink},
		&symlink{Inode: leaf, Target: []byte("saved-target")},
		&xattr{Inode: leaf, Name: "user.saved", Value: []byte("saved-value")},
	); err != nil {
		t.Fatal(err)
	}
	expired.Store(true)
	before := calls.Load()
	var count uint64
	st := m.emptyDir(Background(), parent, true, &count, make(chan int, 1))
	exists, err := m.db.Get(&node{Inode: leaf})
	if err != nil {
		t.Fatal(err)
	}
	remainingAttrs, err := m.db.Where("inode = ?", leaf).Count(&xattr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("emptyDir status=%v removed=%d node_exists=%v xattrs=%d protection_checks=%d", st, count, exists, remainingAttrs, calls.Load()-before)
	if st != syscall.EROFS || count != 0 || !exists || remainingAttrs != 1 || calls.Load() == before {
		t.Fatal("expired protection allowed native trash namespace retirement")
	}
	var target symlink
	if ok, err := m.db.Where("inode = ?", leaf).Get(&target); err != nil || !ok || string(target.Target) != "saved-target" {
		t.Fatalf("trash symlink lost: %+v %v", target, err)
	}
	if ok, err := m.db.Get(&edge{Parent: parent, Name: []byte("expired-link")}); err != nil || !ok {
		t.Fatalf("trash name lost: %v %v", ok, err)
	}
}
