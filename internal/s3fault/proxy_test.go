// SPDX-License-Identifier: AGPL-3.0-only

package s3fault_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// get returns the response to a GET. The caller closes its body.
func get(t *testing.T, proxy *s3fault.Proxy, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.URL()+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
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
	for _, path := range []string{"/bucket/chunks/a", "/bucket/db/a"} {
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
	proxy.RestoreS3()
	if got := status(t, proxy, http.MethodPut, "/bucket/chunks/a"); got != http.StatusOK {
		t.Fatalf("after RestoreS3: status %d", got)
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

func TestErrorCode(t *testing.T) {
	proxy, _ := newProxy(t)
	if err := proxy.SetFault(s3fault.Fault{Status: http.StatusServiceUnavailable, Code: "SlowDown"}); err != nil {
		t.Fatal(err)
	}
	res := get(t, proxy, "/bucket/chunks/a")
	body, err := io.ReadAll(res.Body)
	if err = errors.Join(err, res.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "<Code>SlowDown</Code>") {
		t.Fatalf("throttled GET: status %d, body %q", res.StatusCode, body)
	}
	if err := proxy.SetFault(s3fault.Fault{Code: "SlowDown"}); err == nil {
		t.Fatal("an error code without a status was accepted")
	}
}

func TestCutResponse(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == http.MethodGet {
			if _, err := w.Write([]byte("chunk data")); err != nil {
				t.Error(err)
			}
		}
	}))
	t.Cleanup(upstream.Close)
	proxy, err := s3fault.New(t.Context(), upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = proxy.SetFault(s3fault.Fault{Cut: true}); err != nil {
		t.Fatal(err)
	}
	res := get(t, proxy, "/bucket/chunks/a")
	body, err := io.ReadAll(res.Body)
	if closeErr := res.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(body) != "chunk" {
		t.Fatalf("cut GET body %q, error %v", body, err)
	}
	if _, err := send(t.Context(), proxy, http.MethodPut, "/bucket/chunks/a"); err == nil {
		t.Fatal("cut PUT without a body got a response")
	}
	if calls.Load() != 2 {
		t.Fatalf("%d cut requests reached S3, want 2", calls.Load())
	}
}

func TestMix(t *testing.T) {
	proxy, calls := newProxy(t)
	for _, bad := range []s3fault.Mix{{Fail: -1}, {Fail: 0.6, Cut: 0.6}, {MaxDelay: -time.Second}} {
		if err := proxy.SetMix(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := proxy.SetMix(s3fault.Mix{Fail: 1, MaxDelay: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if got := status(t, proxy, http.MethodPut, "/bucket/chunks/a"); got != http.StatusServiceUnavailable {
			t.Fatalf("mixed PUT: status %d", got)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("failed requests reached S3")
	}
	// A set Fault wins over the mix.
	if err := proxy.SetFault(s3fault.Fault{HeaderDelay: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if got := status(t, proxy, http.MethodPut, "/bucket/chunks/a"); got != http.StatusOK {
		t.Fatalf("with a fault set: status %d", got)
	}
	if err := errors.Join(proxy.SetFault(s3fault.Fault{}), proxy.SetMix(s3fault.Mix{})); err != nil {
		t.Fatal(err)
	}
	if got := status(t, proxy, http.MethodPut, "/bucket/chunks/a"); got != http.StatusOK {
		t.Fatalf("after the mix: status %d", got)
	}
}
