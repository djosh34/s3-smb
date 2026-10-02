// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// Do not unlink the lock file. A new inode would let two processes hold the
// lock. On a stuck shutdown the process exits without unlocking.
type stateLock struct{ file *os.File }

func lockState(dir string) (*stateLock, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	fd, err := syscall.Open(filepath.Join(dir, "state.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), "state.lock")
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("state lock is not a regular file")
	}
	warnPermissions(info, "state lock", 0600)
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("local state is locked by another process (this lock does not protect other hosts)")
	}
	return &stateLock{f}, nil
}

func warnPermissions(info os.FileInfo, kind string, want os.FileMode) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode().Perm() != want || (ok && stat.Uid != uint32(os.Geteuid())) {
		slog.Warn("existing local ownership or permissions differ from private defaults", "file_kind", kind)
	}
}
func (l *stateLock) Close() error { return l.file.Close() }
