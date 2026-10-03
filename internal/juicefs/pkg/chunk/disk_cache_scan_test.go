// SPDX-License-Identifier: AGPL-3.0-only
package chunk

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDiskCacheScanWithStagingCheck(t *testing.T) {
	for _, fast := range []bool{true, false} {
		t.Run(fmt.Sprintf("fast=%t", fast), func(t *testing.T) {
			keys, err := NewKeyIndex(&Config{CacheEviction: Eviction2Random})
			if err != nil {
				t.Fatal(err)
			}
			cache := &cacheStore{
				dir:       t.TempDir(),
				mode:      0600,
				capacity:  1 << 20,
				freeRatio: 0.1,
				keys:      keys,
				pages:     make(map[string]*Page),
				m:         newCacheManagerMetrics(nil),
				opTs:      make(map[time.Duration]func() error),
				uploader: func(key, path string, force bool) bool {
					t.Error("read-cache block must not be uploaded as staging")
					return false
				},
			}
			// Initialize normal I/O state without its unrelated periodic monitor.
			state := &normalDC{}
			state.init(cache)
			cache.state = state
			t.Cleanup(state.stop)

			const key = "chunks/0/0/1_0_521"
			want := bytes.Repeat([]byte{0x5a}, 521)
			if err := cache.flushPage(cache.cachePath(key), want, false, 0); err != nil {
				t.Fatal(err)
			}

			// These run independently in refreshCacheKeys and checkFreeSpace.
			// Join the same operations here without starting their endless loops.
			start := make(chan struct{})
			var workers sync.WaitGroup
			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				cache.scanCached(fast)
			}()
			go func() {
				defer workers.Done()
				<-start
				cache.uploadStaging()
			}()
			close(start)
			workers.Wait()

			if count, used := cache.stats(); count != 1 || used != 521+4096 {
				t.Fatalf("scan lost cached block: count=%d used=%d", count, used)
			}
			reader, err := cache.load(key)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			got := make([]byte, len(want))
			if n, err := reader.ReadAt(got, 0); n != len(want) || err != nil {
				t.Fatalf("cached read: n=%d err=%v", n, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("cached content changed")
			}
		})
	}
}
