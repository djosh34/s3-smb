// SPDX-License-Identifier: AGPL-3.0-only

package s3fault_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

// newProxy starts a proxy in front of a backend that answers 200 and counts
// the requests that reach it.
func newProxy(t *testing.T) (*s3fault.Proxy, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	proxy, err := s3fault.New(t.Context(), upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	return proxy, &calls
}

func send(ctx context.Context, proxy *s3fault.Proxy, method, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, proxy.URL()+path, http.NoBody)
	if err != nil {
		return 0, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	return res.StatusCode, res.Body.Close()
}

func status(t *testing.T, proxy *s3fault.Proxy, method, path string) int {
	t.Helper()
	code, err := send(t.Context(), proxy, method, path)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestStatusFault(t *testing.T) {
	proxy, calls := newProxy(t)
	if err := proxy.SetFault(s3fault.Fault{Method: http.MethodGet, Status: http.StatusServiceUnavailable}); err != nil {
		t.Fatal(err)
	}
	if got := status(t, proxy, http.MethodGet, "/bucket/chunks/a"); got != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("faulted GET: status %d, %d requests reached S3", got, calls.Load())
	}
	if got := status(t, proxy, http.MethodPut, "/bucket/chunks/a"); got != http.StatusOK {
		t.Fatalf("PUT does not match the fault but got status %d", got)
	}
	if err := proxy.SetFault(s3fault.Fault{}); err != nil {
		t.Fatal(err)
	}
	if got := status(t, proxy, http.MethodGet, "/bucket/chunks/a"); got != http.StatusOK {
		t.Fatalf("zero fault: status %d", got)
	}
}

func TestOutage(t *testing.T) {
	proxy, calls := newProxy(t)
	proxy.FailS3For(time.Hour)
	for _, path := range []string{"/bucket/chunks/a", "/bucket/meta/a"} {
		if got := status(t, proxy, http.MethodPut, path); got != http.StatusServiceUnavailable {
			t.Fatalf("PUT %s during outage: status %d", path, got)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("requests reached S3 during the outage")
	}
	if event := <-proxy.OutageSeen(); event.Path != "/bucket/chunks/a" {
		t.Fatalf("outage event %+v", event)
	}
	if event := <-proxy.MetadataFailureSeen(); event.Path != "/bucket/meta/a" {
		t.Fatalf("metadata outage event %+v", event)
	}
	proxy.RestoreS3()
	if got := status(t, proxy, http.MethodPut, "/bucket/chunks/a"); got != http.StatusOK {
		t.Fatalf("after RestoreS3: status %d", got)
	}
}

func TestMetadataFailure(t *testing.T) {
	proxy, _ := newProxy(t)
	proxy.SetMetadataFailure(true)
	if got := status(t, proxy, http.MethodPut, "/bucket/meta/a"); got != http.StatusServiceUnavailable {
		t.Fatalf("metadata PUT: status %d", got)
	}
	if event := <-proxy.MetadataFailureSeen(); event.Method != http.MethodPut {
		t.Fatalf("metadata event %+v", event)
	}
	if got := status(t, proxy, http.MethodPut, "/bucket/chunks/a"); got != http.StatusOK {
		t.Fatalf("chunk PUT during metadata failure: status %d", got)
	}
	proxy.SetMetadataFailure(false)
	if got := status(t, proxy, http.MethodPut, "/bucket/meta/a"); got != http.StatusOK {
		t.Fatalf("metadata PUT after reset: status %d", got)
	}
}

func TestHeldChunkResponse(t *testing.T) {
	proxy, calls := newProxy(t)
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	status(t, proxy, http.MethodPut, "/bucket/meta/a")
	done := make(chan error, 1)
	go func() {
		code, err := send(t.Context(), proxy, http.MethodPut, "/bucket/chunks/a")
		if err == nil && code != http.StatusOK {
			err = fmt.Errorf("released PUT: status %d", code)
		}
		done <- err
	}()
	<-held
	if calls.Load() != 2 || proxy.ChunkPuts() != 1 {
		t.Fatalf("held PUT: %d requests reached S3, %d chunk PUTs counted", calls.Load(), proxy.ChunkPuts())
	}
	select {
	case <-done:
		t.Fatal("held response reached the caller before Release")
	default:
	}
	proxy.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
