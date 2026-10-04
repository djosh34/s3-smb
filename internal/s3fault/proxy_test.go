// SPDX-License-Identifier: AGPL-3.0-only

package s3fault_test

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

func newProxy(t *testing.T, handler http.Handler) *s3fault.Proxy {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	proxy, err := s3fault.New(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	return proxy
}

func request(t *testing.T, ctx context.Context, proxy *s3fault.Proxy, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, proxy.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := res.Body.Close(); err != nil {
			t.Error(err)
		}
	})
	return res
}

func setFault(t *testing.T, proxy *s3fault.Proxy, fault s3fault.Fault) {
	t.Helper()
	if err := proxy.SetFault(fault); err != nil {
		t.Fatal(err)
	}
}

func payload(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "10")
	if _, err := io.WriteString(w, "0123456789"); err != nil {
		// The cancellation tests deliberately disconnect during responses.
		return
	}
}

func TestForwardSignedRequest(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "signed.test:1234" || r.Method != http.MethodPut || r.URL.RequestURI() != "/bucket/chunks/key?partNumber=2" || r.Header.Get("Authorization") != "test signature" {
			t.Errorf("signed request was changed: %s %s %s", r.Host, r.Method, r.URL.RequestURI())
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if string(data) != "fixture" {
			t.Errorf("body = %q", data)
		}
		w.Header().Set("ETag", "test-etag")
		w.WriteHeader(http.StatusCreated)
	}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, proxy.URL()+"/bucket/chunks/key?partNumber=2", strings.NewReader("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "signed.test:1234"
	req.Header.Set("Authorization", "test signature")
	res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated || res.Header.Get("ETag") != "test-etag" || proxy.ChunkPuts() != 1 {
		t.Fatalf("forwarded status=%d etag=%q puts=%d", res.StatusCode, res.Header.Get("ETag"), proxy.ChunkPuts())
	}
}

func TestResponseDelays(t *testing.T) {
	const delay = 80 * time.Millisecond
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			proxy := newProxy(t, http.HandlerFunc(payload))
			fault := s3fault.Fault{}
			if phase == "headers" {
				fault.HeaderDelay = delay
			} else {
				fault.BodyDelay = delay
			}
			setFault(t, proxy, fault)
			start := time.Now()
			res := request(t, t.Context(), proxy, http.MethodGet, "/bucket/chunks/key")
			if phase == "headers" && time.Since(start) < delay {
				t.Fatal("headers arrived before the configured delay")
			}
			bodyStart := time.Now()
			data, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "body" && time.Since(bodyStart) < delay {
				t.Fatal("body arrived before the configured delay")
			}
			if string(data) != "0123456789" {
				t.Fatalf("delayed body = %q", data)
			}
		})
	}
}

func TestInjectedErrorsAndThrottling(t *testing.T) {
	var calls atomic.Int64
	proxy := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, fault := range []s3fault.Fault{
		{Status: http.StatusInternalServerError, Code: "InternalError"},
		{Status: http.StatusServiceUnavailable, Code: "SlowDown"},
		{Status: http.StatusTooManyRequests, Code: "SlowDown"},
		{Status: http.StatusServiceUnavailable},
	} {
		setFault(t, proxy, fault)
		res := request(t, t.Context(), proxy, http.MethodPut, "/bucket/chunks/key")
		var body struct{ Code string }
		if err := xml.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		wantCode := fault.Code
		if wantCode == "" {
			wantCode = "ServiceUnavailable"
		}
		if res.StatusCode != fault.Status || body.Code != wantCode || res.Header.Get("Content-Type") != "application/xml" {
			t.Fatalf("injected response status=%d code=%q", res.StatusCode, body.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("injected requests reached S3")
	}
	setFault(t, proxy, s3fault.Fault{})
	if res := request(t, t.Context(), proxy, http.MethodPut, "/bucket/chunks/key"); res.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d", res.StatusCode)
	}
}

func TestFaultMatchingAndValidation(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	setFault(t, proxy, s3fault.Fault{Method: http.MethodGet, PathContains: "/chunks/", Status: http.StatusServiceUnavailable})
	for _, fault := range []s3fault.Fault{
		{HeaderDelay: -1}, {BodyDelay: -1}, {CutAfter: -1}, {Status: 200}, {Status: 600},
	} {
		if err := proxy.SetFault(fault); err == nil {
			t.Fatalf("accepted invalid fault: %+v", fault)
		}
	}
	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/bucket/chunks/key", http.StatusServiceUnavailable},
		{http.MethodPut, "/bucket/chunks/key", http.StatusOK},
		{http.MethodGet, "/bucket/meta/key", http.StatusOK},
	} {
		if res := request(t, t.Context(), proxy, test.method, test.path); res.StatusCode != test.status {
			t.Fatalf("%s %s: status = %d", test.method, test.path, res.StatusCode)
		}
	}
}

func TestCutBodies(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	for _, cut := range []int64{0, 1, 4, 9, 10, 20} {
		setFault(t, proxy, s3fault.Fault{CutBody: true, CutAfter: cut})
		res := request(t, t.Context(), proxy, http.MethodGet, "/bucket/chunks/key")
		data, err := io.ReadAll(res.Body)
		want := min(cut, 10)
		if string(data) != "0123456789"[:want] {
			t.Fatalf("cut %d: body = %q", cut, data)
		}
		if cut < 10 && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut %d: error = %v, want unexpected EOF", cut, err)
		}
		if cut >= 10 && err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if res.ContentLength != 10 {
			t.Fatalf("cut changed content length to %d", res.ContentLength)
		}
	}
}

func TestTimedOutage(t *testing.T) {
	var calls atomic.Int64
	proxy := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	start := proxy.FailS3For(100 * time.Millisecond)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if res := request(t, t.Context(), proxy, method, "/bucket/chunks/key"); res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("outage status = %d", res.StatusCode)
		}
		select {
		case event := <-proxy.OutageSeen():
			if event.Method != method || event.Path != "/bucket/chunks/key" || event.Status != http.StatusServiceUnavailable {
				t.Fatalf("outage event = %+v", event)
			}
		default:
			t.Fatal("outage was not observed")
		}
	}
	if res := request(t, t.Context(), proxy, http.MethodGet, "/bucket/meta/key"); res.StatusCode != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatal("outage did not block all S3 requests")
	}
	time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
	if res := request(t, t.Context(), proxy, http.MethodGet, "/bucket/chunks/key"); res.StatusCode != http.StatusNoContent {
		t.Fatal("outage outlasted its deadline")
	}
	proxy.FailS3For(time.Minute)
	proxy.RestoreS3()
	if res := request(t, t.Context(), proxy, http.MethodGet, "/bucket/chunks/key"); res.StatusCode != http.StatusNoContent {
		t.Fatal("RestoreS3 did not end outage")
	}
	proxy.FailS3For(0)
	if res := request(t, t.Context(), proxy, http.MethodGet, "/bucket/chunks/key"); res.StatusCode != http.StatusNoContent {
		t.Fatal("zero duration caused outage")
	}
}

func TestMetadataFailure(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	proxy.SetMetadataFailure(true)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if res := request(t, t.Context(), proxy, method, "/bucket/meta/key"); res.StatusCode != http.StatusServiceUnavailable {
			t.Fatal("metadata failure did not reach caller")
		}
		select {
		case event := <-proxy.MetadataFailureSeen():
			if event.Method != method || event.Path != "/bucket/meta/key" || event.Status != http.StatusServiceUnavailable {
				t.Fatalf("metadata event = %+v", event)
			}
		default:
			t.Fatal("metadata failure was not observed")
		}
	}
	for _, test := range []struct{ method, path string }{
		{http.MethodHead, "/bucket/meta/key"}, {http.MethodGet, "/bucket/chunks/key"},
	} {
		if res := request(t, t.Context(), proxy, test.method, test.path); res.StatusCode != http.StatusOK {
			t.Fatal("metadata failure affected an unrelated request")
		}
	}
	proxy.SetMetadataFailure(false)
	if res := request(t, t.Context(), proxy, http.MethodGet, "/bucket/meta/key"); res.StatusCode != http.StatusOK {
		t.Fatal("metadata failure did not reset")
	}
}

func TestHeldChunkResponse(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.HoldNextChunkResponse(); err == nil {
		t.Fatal("overwrote an armed hold")
	}
	// These requests must neither consume the hold nor count as chunk PUTs.
	request(t, t.Context(), proxy, http.MethodGet, "/bucket/chunks/key")
	request(t, t.Context(), proxy, http.MethodPut, "/bucket/meta/key")
	done := make(chan *http.Response, 1)
	go func() { done <- request(t, t.Context(), proxy, http.MethodPut, "/bucket/chunks/key") }()
	select {
	case event := <-held:
		if event.Method != http.MethodPut || event.Status != http.StatusOK || proxy.ChunkPuts() != 1 {
			t.Fatalf("held event = %+v, puts = %d", event, proxy.ChunkPuts())
		}
	case <-time.After(time.Second):
		t.Fatal("chunk response was not observed")
	}
	select {
	case <-done:
		t.Fatal("held response reached caller before Release")
	default:
	}
	proxy.Release()
	select {
	case res := <-done:
		if res.StatusCode != http.StatusOK {
			t.Fatalf("released status = %d", res.StatusCode)
		}
	case <-time.After(time.Second):
		t.Fatal("Release did not unblock caller")
	}
	proxy.Release()
	if _, err := proxy.HoldNextChunkResponse(); err != nil {
		t.Fatal(err)
	}
	proxy.Release()
}

func TestInvalidUpstream(t *testing.T) {
	for _, upstream := range []string{"", "://", "http:///missing", "https://example.test", "http://user:secret@example.test", "http://example.test?token=secret", "http://example.test/#fragment"} {
		proxy, err := s3fault.New(upstream)
		if err == nil {
			if err := proxy.Close(); err != nil {
				t.Error(err)
			}
			t.Fatalf("accepted upstream %q", upstream)
		}
	}
}

func TestConcurrentFaultChanges(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 20 {
				setFault(t, proxy, s3fault.Fault{Status: http.StatusServiceUnavailable})
				proxy.SetMetadataFailure(true)
				proxy.FailS3For(time.Millisecond)
				request(t, t.Context(), proxy, http.MethodGet, "/bucket/chunks/key")
				proxy.RestoreS3()
				proxy.SetMetadataFailure(false)
				setFault(t, proxy, s3fault.Fault{})
			}
		})
	}
	workers.Wait()
}
