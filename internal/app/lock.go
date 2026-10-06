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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the data folder: %w", err)
	}
	fd, err := syscall.Open(filepath.Join(dir, "state.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the folder lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), "state.lock")
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(errors.New("the folder lock is not a regular file"), f.Close())
	}
	warnPermissions(info, "folder lock", 0o600)
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(errors.New("the data folder is locked by another process"), f.Close())
	}
	return &stateLock{f}, nil
}

func warnPermissions(info os.FileInfo, kind string, want os.FileMode) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode().Perm() != want || (ok && int(stat.Uid) != os.Geteuid()) {
		slog.Warn("existing local ownership or permissions differ from private defaults", "file_kind", kind)
	}
}

func (l *stateLock) Close() error { return l.file.Close() }
