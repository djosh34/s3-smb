// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// Ordinary writes only: no malformed SMB, allocator misuse, or artificial
// production scheduling hooks. Each worker owns a file and source buffer.
// A block >64KiB makes wSlice assemble its first block from pooled small pages;
// short tails also exercise the asynchronous cache path used by the application.
func TestPagePoolOrdinaryWrites(t *testing.T) {
	for _, mode := range []string{"disabled", "memory", "disk", "disk-lz4", "disk-zstd"} {
		t.Run(mode, func(t *testing.T) {
			cc := chunk.Config{BlockSize: 256 << 10, MaxUpload: 2, MaxDownload: 2,
				BufferSize: 32 << 20, CacheMode: 0600, CacheChecksum: chunk.CsExtend, FreeSpace: 0.001,
				CacheScanInterval: time.Hour, AutoCreate: true, Compress: "none",
				MaxRetries: 1, GetTimeout: 3 * time.Second, PutTimeout: 3 * time.Second}
			if mode != "disabled" {
				cc.CacheSize = 4 << 20
				cc.CacheFullBlock = true
				cc.CacheDir = "memory"
			}
			if mode == "disk" || mode == "disk-lz4" || mode == "disk-zstd" {
				cc.CacheDir = filepath.Join(t.TempDir(), "cache")
			}
			if mode == "disk-lz4" {
				cc.Compress = "lz4"
			}
			if mode == "disk-zstd" {
				cc.Compress = "zstd"
			}
			cc.SelfCheck("page-pool-test")
			disk, err := object.CreateStorage("file", filepath.Join(t.TempDir(), "objects")+"/", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			s := fixtureWithChunkConfig(t, &failingStore{ObjectStorage: disk}, nil, &cc).s
			const workers, rounds = 4, 8
			var wg sync.WaitGroup
			failures := make(chan error, workers)
			for worker := 0; worker < workers; worker++ {
				h := openFile(t, s, fmt.Sprintf("ordinary-%d", worker))
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer s.Close(h)
					for round := 0; round < rounds; round++ {
						// Nonaligned writes, overwrite within a page, and an explicit
						// sparse gap catch stale bytes surviving from pooled pages.
						const length = 2*(256<<10) + 12345
						want := make([]byte, length)
						if err := s.Truncate(h, 0); err != nil {
							failures <- err
							return
						}
						writes := [][2]int{{0, 65535}, {65535, 32769}, {98304, 425984}, {length - 71, 71}, {37, 521}}
						for index, write := range writes {
							off, size := write[0], write[1]
							data := make([]byte, size)
							for i := range data {
								data[i] = byte(i*17 + worker*29 + round*7 + index)
							}
							copy(want[off:], data)
							n, err := s.Write(h, data, uint64(off), 0)
							if err != nil || n != len(data) {
								failures <- fmt.Errorf("write: n=%d err=%v", n, err)
								return
							}
							// Reuse caller's buffer as soon as Write has returned.
							for i := range data {
								data[i] = 0xd7
							}
						}
						if err := s.Flush(h); err != nil {
							failures <- err
							return
						}
						got := make([]byte, len(want))
						n, err := s.Read(h, got, 0, 0)
						if err != nil || n != len(want) || !bytes.Equal(got, want) {
							first, last, count := -1, -1, 0
							for i := range want {
								if got[i] != want[i] {
									if first < 0 {
										first = i
									}
									last = i
									count++
								}
							}
							failures <- fmt.Errorf("worker %d round %d content mismatch: n=%d err=%v first=%d last=%d count=%d", worker, round, n, err, first, last, count)
							return
						}
					}
				}()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
		})
	}
}
