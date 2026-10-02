// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"context"
	"errors"
	"math"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func lockType(t vfs.ByteRangeLockType) uint32 {
	switch t {
	case vfs.ByteRangeLockShared:
		return syscall.F_RDLCK
	case vfs.ByteRangeLockExclusive:
		return syscall.F_WRLCK
	default:
		return syscall.F_UNLCK
	}
}
func (s *FS) Lock(h vfs.VfsHandle, locks []vfs.ByteRangeLock) error {
	return s.LockContext(context.Background(), h, locks)
}

// LockContext is Lock with cancellation. JuiceFS Setlk decides conflicts.
// LockContext polls a nonblocking Setlk, so a wait for another SMB handle ends
// on cancellation or shutdown.
func (s *FS) LockContext(ctx context.Context, h vfs.VfsHandle, locks []vfs.ByteRangeLock) error {
	if len(locks) == 0 {
		return syscall.EINVAL
	}
	for _, l := range locks {
		if l.Length == 0 || l.Offset > math.MaxUint64-(l.Length-1) || l.Type < vfs.ByteRangeLockShared || l.Type > vfs.ByteRangeLockUnlock {
			return syscall.EINVAL
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.mu.Lock()
		f, e := s.get(h)
		if e != nil {
			s.mu.Unlock()
			return e
		}
		done := f.done
		e, wait := s.tryLocks(h, f, locks)
		s.mu.Unlock()
		if e == nil || !wait {
			return e
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-done:
			timer.Stop()
			return syscall.EBADF
		case <-timer.C:
		}
	}
}
func (s *FS) tryLocks(h vfs.VfsHandle, f *handle, locks []vfs.ByteRangeLock) (error, bool) {
	desired := append([]vfs.ByteRangeLock(nil), f.locks...)
	for _, l := range locks {
		if l.Type == vfs.ByteRangeLockUnlock {
			index := -1
			for i, held := range desired {
				if held.Offset == l.Offset && held.Length == l.Length {
					index = i
					break
				}
			}
			if index < 0 {
				return syscall.ENOLCK, false
			}
			desired = append(desired[:index], desired[index+1:]...)
		} else {
			typ := lockType(l.Type)
			start, end := l.Offset, l.Offset+l.Length-1
			var pid uint32
			if er := s.meta.Getlk(s.ctx, f.file.Inode(), uint64(h), &typ, &start, &end, &pid); er != 0 {
				return er, false
			}
			if typ != syscall.F_UNLCK {
				return syscall.EAGAIN, !l.FailImmediately
			}
			desired = append(desired, l)
		}
	}
	// SMB keeps each locked range and JuiceFS merges adjacent POSIX locks. After
	// an unlock, set the JuiceFS locks again from the ranges that remain.
	if e := s.replaceLocks(h, f, desired); e != nil {
		restore := s.replaceLocks(h, f, f.locks)
		return errors.Join(e, restore), false
	}
	f.locks = desired
	return nil, false
}
func (s *FS) replaceLocks(h vfs.VfsHandle, f *handle, locks []vfs.ByteRangeLock) error {
	f.lockOwnerUsed = true // close must unlock even when this batch fails part-way
	if er := s.meta.Setlk(s.ctx, f.file.Inode(), uint64(h), false, syscall.F_UNLCK, 0, math.MaxUint64, 1); er != 0 {
		return er
	}
	// Shared first, exclusive last, so overlapping shared ranges cannot weaken an
	// exclusive range belonging to the same SMB handle.
	for _, typ := range []vfs.ByteRangeLockType{vfs.ByteRangeLockShared, vfs.ByteRangeLockExclusive} {
		for _, l := range locks {
			if l.Type == typ {
				if er := s.meta.Setlk(s.ctx, f.file.Inode(), uint64(h), false, lockType(l.Type), l.Offset, l.Offset+l.Length-1, 1); er != 0 {
					return er
				}
			}
		}
	}
	return nil
}
func (s *FS) checkIO(h vfs.VfsHandle, f *handle, off uint64, length int, write bool) error {
	if length == 0 {
		return nil
	}
	typ := uint32(syscall.F_RDLCK)
	if write {
		typ = syscall.F_WRLCK
	}
	end := off + uint64(length) - 1
	var pid uint32
	if er := s.meta.Getlk(s.ctx, f.file.Inode(), uint64(h), &typ, &off, &end, &pid); er != 0 {
		return er
	}
	if typ != syscall.F_UNLCK {
		return syscall.EAGAIN
	}
	return nil
}
