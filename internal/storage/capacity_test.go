// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func TestFilesystemCapacityEnforcement(t *testing.T) {
	for _, capacity := range []uint64{16 * 1024, 0} {
		name := "limited"
		if capacity == 0 {
			name = "unlimited"
		}
		t.Run(name, func(t *testing.T) {
			format, err := NewFormat("test", false, 14)
			if err != nil {
				t.Fatal(err)
			}
			format.Capacity = capacity
			conf := meta.DefaultConf()
			conf.NoBGJob = true
			m, err := OpenMetadata(filepath.Join(t.TempDir(), "metadata.db"), conf)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := m.Shutdown(); err != nil {
					t.Error(err)
				}
			})
			if err = m.Init(format, false); err != nil {
				t.Fatal(err)
			}
			raw, err := object.CreateStorage("file", t.TempDir(), "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			runtime, err := OpenFilesystem(m, raw, format, t.TempDir(), &zero, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := runtime.Close(); err != nil {
					t.Error(err)
				}
			})
			if err = m.NewSession(false); err != nil {
				t.Fatal(err)
			}
			ctx := meta.NewContext(1, 0, []uint32{0})
			handle, errno := runtime.FS.Create(ctx, "/fixture", 0600, 0)
			if errno != 0 {
				t.Fatal(errno)
			}
			t.Cleanup(func() {
				if errno := handle.Close(ctx); errno != 0 && !errors.Is(errno, syscall.ENOSPC) {
					t.Error(errno)
				}
			})
			data := bytes.Repeat([]byte("a"), 4096)
			if n, errno := handle.Write(ctx, data); errno != 0 || n != len(data) {
				t.Fatalf("write below capacity: %d, %v", n, errno)
			}
			if errno := handle.Fsync(ctx); errno != 0 {
				t.Fatalf("flush below capacity: %v", errno)
			}
			// JuiceFS checks allocated space when buffered writes are committed.
			data = bytes.Repeat([]byte("b"), 16*1024)
			if n, errno := handle.Write(ctx, data); errno != 0 || n != len(data) {
				t.Fatalf("buffer write: %d, %v", n, errno)
			}
			errno = handle.Fsync(ctx)
			if capacity == 0 {
				if errno != 0 {
					t.Fatalf("unlimited write failed: %v", errno)
				}
			} else if !errors.Is(errno, syscall.ENOSPC) || errno.Error() != "no space left on device" {
				t.Fatalf("write beyond capacity: got %v, want no space left on device", errno)
			}
		})
	}
}
