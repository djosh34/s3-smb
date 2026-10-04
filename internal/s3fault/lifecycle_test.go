// SPDX-License-Identifier: AGPL-3.0-only

package s3fault

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// enteredContext signals when the handler starts waiting, not when a client
// starts its request. It lets the test interrupt an active delay without sleeps.
type enteredContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
	armed   atomic.Bool
}

func (ctx *enteredContext) Done() <-chan struct{} {
	if ctx.armed.Load() {
		ctx.once.Do(func() { close(ctx.entered) })
	}
	return ctx.Context.Done()
}

type headerRecorder struct {
	*httptest.ResponseRecorder
	ctx *enteredContext
}

func (recorder *headerRecorder) WriteHeader(status int) {
	recorder.ResponseRecorder.WriteHeader(status)
	// Arm body-delay observation only after the response headers are written.
	recorder.ctx.armed.Store(true)
}

type responseTransport struct {
	ctx   *enteredContext
	phase string
}

func (transport responseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Ignore ReverseProxy's early context check and observe the header delay
	// only after the upstream round trip has completed.
	transport.ctx.armed.Store(transport.phase == "headers")
	return &http.Response{
		Request: req, StatusCode: http.StatusOK, ContentLength: 10,
		Header: http.Header{"Content-Length": {"10"}},
		Body:   io.NopCloser(strings.NewReader("0123456789")),
	}, nil
}

func TestCancelAndClose(t *testing.T) {
	for _, phase := range []string{"headers", "body", "hold", "injected"} {
		for _, action := range []string{"cancel", "close"} {
			t.Run(phase+"/"+action, func(t *testing.T) {
				testInterruption(t, phase, action)
			})
		}
	}
}

func testInterruption(t *testing.T, phase, action string) {
	t.Helper()
	proxy, err := New(t.Context(), "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &enteredContext{Context: ctx, entered: make(chan struct{})}
	fault := Fault{HeaderDelay: time.Hour}
	if phase == "body" {
		fault = Fault{BodyDelay: time.Hour}
	}
	if phase == "injected" {
		fault.Status = http.StatusServiceUnavailable
		observed.armed.Store(true)
	}
	if err := proxy.SetFault(fault); err != nil {
		t.Fatal(err)
	}
	var held <-chan Event
	if phase == "hold" {
		held, err = proxy.HoldNextChunkResponse()
		if err != nil {
			t.Fatal(err)
		}
	}
	reverse := &httputil.ReverseProxy{
		Rewrite:   func(_ *httputil.ProxyRequest) {},
		Transport: responseTransport{ctx: observed, phase: phase},
		ErrorLog:  log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			proxy.writeError(w, http.StatusBadGateway, "ServiceUnavailable")
		},
	}
	req := httptest.NewRequestWithContext(observed, http.MethodPut, "/bucket/chunks/key", nil)
	recorder := &headerRecorder{ResponseRecorder: httptest.NewRecorder(), ctx: observed}
	finished := make(chan struct{})
	go func() {
		// Keep the writer alive during Close so a false 200 cannot be hidden
		// by the server closing its connection. Exercise the real fault handler.
		proxy.handler(reverse).ServeHTTP(recorder, req)
		close(finished)
	}()
	select {
	case <-observed.entered:
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("handler did not enter its fault")
	}
	if action == "cancel" {
		cancel()
	} else if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("interrupted handler remained blocked")
	}
	if phase == "body" {
		if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
			t.Fatal("body delay did not stop the response after its headers")
		}
	} else if recorder.Code != http.StatusBadGateway {
		t.Fatalf("interrupted %s status = %d, want 502", phase, recorder.Code)
	}
}
