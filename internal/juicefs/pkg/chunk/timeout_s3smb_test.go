// SPDX-License-Identifier: AGPL-3.0-only
// Modified for s3-smb, 2026. See docs/vendored.md.

package chunk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/compress"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/utils"
)

// lateGetStore models an object store that finishes GET after cancellation.
type lateGetStore struct {
	object.ObjectStorage
	started chan struct{}
	closed  chan struct{}
}

func (s *lateGetStore) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	close(s.started)
	<-ctx.Done()
	// Keep the late result unordered with the caller's return.
	time.Sleep(10 * time.Millisecond)
	attrs := object.ApplyGetters(getters...)
	attrs.SetRequestID("late-get").SetStorageClass("late-class")
	return &lateGetBody{Reader: bytes.NewReader([]byte("data")), closed: s.closed}, nil
}

type lateGetBody struct {
	*bytes.Reader
	closed chan struct{}
}

func (b *lateGetBody) Close() error {
	close(b.closed)
	return nil
}

func newTimeoutTestStore(storage object.ObjectStorage) *cachedStore {
	store := &cachedStore{
		storage:         storage,
		conf:            Config{GetTimeout: 5 * time.Millisecond},
		compressor:      compress.NewCompressor("none"),
		currentDownload: make(chan struct{}, 1),
		group:           NewController(),
		fetcher:         newPrefetcher(0, nil),
	}
	store.initMetrics()
	return store
}

func TestCachedStoreLateGetResult(t *testing.T) {
	for _, kind := range []string{"full", "range"} {
		for _, cancelParent := range []bool{false, true} {
			name := kind + "/timeout"
			if cancelParent {
				name = kind + "/cancel"
			}
			t.Run(name, func(t *testing.T) {
				storage := &lateGetStore{started: make(chan struct{}), closed: make(chan struct{})}
				store := newTimeoutTestStore(storage)
				page := NewPage(make([]byte, 4))
				defer page.Release()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if cancelParent {
					go func() {
						<-storage.started
						cancel()
					}()
				}
				var err error
				if kind == "full" {
					err = store.load(ctx, "chunks/0/0/1_0_4", page, false, false)
				} else {
					_, err = store.loadRange(ctx, "chunks/0/0/1_0_4", page, 0)
				}
				<-storage.closed
				// Wait for the callback's deferred page release before test cleanup.
				deadline := time.Now().Add(time.Second)
				for atomic.LoadInt32(&page.refs) != 1 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if atomic.LoadInt32(&page.refs) != 1 {
					t.Fatal("GET callback did not release its page")
				}
				if cancelParent {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("want cancellation, got %v", err)
					}
				} else if kind == "range" {
					if !errors.Is(err, utils.ErrFuncTimeout) {
						t.Fatalf("want timeout without full-read fallback, got %v", err)
					}
				} else if err == nil {
					t.Fatal("late GET must not turn a timeout into success")
				}
			})
		}
	}
}

func TestCachedStoreGetResults(t *testing.T) {
	storage, err := object.CreateStorage("mem", t.Name(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Put(context.Background(), "block", bytes.NewReader([]byte("data"))); err != nil {
		t.Fatal(err)
	}
	store := newTimeoutTestStore(storage)
	store.conf.GetTimeout = time.Second
	for _, kind := range []string{"full", "range"} {
		t.Run(kind, func(t *testing.T) {
			page := NewPage(make([]byte, 4))
			defer page.Release()
			if kind == "full" {
				err = store.load(context.Background(), "block", page, false, false)
			} else {
				var n int
				n, err = store.loadRange(context.Background(), "block", page, 0)
				if n != 4 {
					t.Fatalf("want 4 bytes, got %d", n)
				}
			}
			if err != nil || string(page.Data) != "data" {
				t.Fatalf("GET: data=%q err=%v", page.Data, err)
			}
			if kind == "full" {
				err = store.load(context.Background(), "missing", page, false, false)
				if err == nil {
					t.Fatal("missing object must fail")
				}
			} else {
				_, err = store.loadRange(context.Background(), "missing", page, 0)
				if !errors.Is(err, errTryFullRead) {
					t.Fatalf("ordinary GET error must allow full-read fallback, got %v", err)
				}
			}
		})
	}
}
