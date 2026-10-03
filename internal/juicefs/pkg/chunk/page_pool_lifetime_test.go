// SPDX-License-Identifier: AGPL-3.0-only
package chunk

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPagePoolRetainedOwners(t *testing.T) {
	root := NewOffPage(pageSize)
	for i := range root.Data {
		root.Data[i] = byte(i*19 + 5)
	}
	want := bytes.Clone(root.Data[11 : pageSize-13])
	child := root.Slice(11, len(want))
	reader := NewPageReader(child)
	root.Release()
	child.Release()
	if n := atomic.LoadInt32(&root.refs); n != 1 {
		t.Fatalf("root refs=%d", n)
	}
	if n := atomic.LoadInt32(&child.refs); n != 1 {
		t.Fatalf("child refs=%d", n)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 128; i++ {
				p := NewOffPage(pageSize)
				for j := range p.Data {
					p.Data[j] = 0xa5
				}
				p.Release()
			}
		}()
	}
	got, err := io.ReadAll(reader)
	wg.Wait()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("retained data changed: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if root.Data != nil || child.Data != nil {
		t.Fatal("final release did not clear pages")
	}
	if atomic.LoadInt32(&root.refs) != 0 || atomic.LoadInt32(&child.refs) != 0 {
		t.Fatal("unbalanced ownership")
	}
}

func TestPagePoolMemoryCacheReaderSurvivesEviction(t *testing.T) {
	store := NewCachedStore(nil, Config{CacheDir: "memory", CacheSize: pageSize, BlockSize: pageSize,
		MaxUpload: 1, MaxDownload: 1}, nil).(*cachedStore)
	page := NewOffPage(pageSize)
	for i := range page.Data {
		page.Data[i] = 0x69
	}
	store.bcache.cache("retained", page, false, false)
	page.Release()
	reader, err := store.bcache.load("retained")
	if err != nil {
		t.Fatal(err)
	}
	store.bcache.remove("retained", false)
	for i := 0; i < 128; i++ {
		churn := NewOffPage(pageSize)
		for j := range churn.Data {
			churn.Data[j] = 0x96
		}
		churn.Release()
	}
	got := make([]byte, pageSize)
	n, err := reader.ReadAt(got, 0)
	if err != nil || n != len(got) || !bytes.Equal(got, bytes.Repeat([]byte{0x69}, pageSize)) {
		t.Fatalf("reader invalid after eviction: n=%d err=%v", n, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&page.refs) != 0 {
		t.Fatal("unbalanced cache/reader ownership")
	}
}

func TestPagePoolChannelPreservesOwnership(t *testing.T) {
	// This separate channel pool owns Page objects without reducing refs; unlike
	// utils.Free's sync.Pool, channel insertion cannot randomly discard entries.
	for {
		select {
		case p := <-pagePool:
			p.Release()
		default:
			goto empty
		}
	}
empty:
	page := allocPage(pageSize)
	page.Data[0] = 0x5e
	freePage(page)
	reused := allocPage(pageSize)
	if reused != page || atomic.LoadInt32(&reused.refs) != 1 || reused.Data[0] != 0x5e {
		t.Fatal("channel did not transfer sole live reference")
	}
	reused.Release()
}
