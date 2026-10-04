// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWriteFullSync(t *testing.T) {
	injected := errors.New("full sync failed")
	for _, syncErr := range []error{nil, injected} {
		t.Run("sync", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			var descriptor uintptr
			calls := 0
			err := writeFullSync(path, func(fd uintptr) error {
				calls++
				descriptor = fd
				data, err := os.ReadFile(path) //nolint:gosec // This is the fixed fixture path under t.TempDir.
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "F_FULLFSYNC acceptance\n" {
					t.Fatal("sync ran before the write", string(data))
				}
				return syncErr
			})
			if !errors.Is(err, syncErr) || calls != 1 {
				t.Fatal("wrong sync result", calls, err)
			}
			if _, err := unix.FcntlInt(descriptor, unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
				t.Fatal("file descriptor not closed", err)
			}
		})
	}
}

func TestWriteFullSyncOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := writeFullSync(path, func(uintptr) error {
		called = true
		return nil
	})
	if !errors.Is(err, os.ErrExist) || called {
		t.Fatal("existing file must fail before sync", called, err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // This is the fixed fixture path under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatal("existing file changed", string(data))
	}
}
