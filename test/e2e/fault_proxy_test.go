// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// faultEvent deliberately excludes headers and bodies: signed requests contain
// credentials. These observations can also establish the interruption point in
// the final Mac test; they do not themselves claim Time Machine acceptance.
type faultEvent struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	Method string    `json:"method,omitempty"`
	Path   string    `json:"path,omitempty"`
	Status int       `json:"status,omitempty"`
}
type heldResponse struct {
	seen    chan faultEvent
	release chan struct{}
	once    sync.Once
}
type faultProxy struct {
	server          *httptest.Server
	mu              sync.Mutex
	next, active    *heldResponse
	events          *os.File
	eventMu         sync.Mutex
	metadataFailure atomic.Bool
	metadataSeen    chan faultEvent
	chunkPuts       atomic.Int64 // Successful real data PUTs, no headers/bodies retained.
}

func newFaultProxy(t *testing.T, upstream string) *faultProxy {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" || target.Scheme != "http" {
		t.Fatal("fault proxy requires explicit disposable HTTP MinIO")
	}
	artifacts := os.Getenv("S3_SMB_TEST_ARTIFACTS")
	if artifacts == "" {
		artifacts = t.TempDir()
	}
	dir := filepath.Join(artifacts, strings.ReplaceAll(t.Name(), "/", "-"))
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	events, err := os.OpenFile(filepath.Join(dir, "proxy-events.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{events: events, metadataSeen: make(chan faultEvent, 32)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// NewSingleHostReverseProxy preserves the signed request Host, including the
	// loopback proxy port; only the upstream dial address changes.
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.record(faultEvent{Event: "upstream_error", Method: r.Method, Path: r.URL.Path})
		http.Error(w, "test upstream unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(res *http.Response) error {
		if res.Request.Method != http.MethodPut || !strings.Contains(res.Request.URL.Path, "/chunks/") || res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil
		}
		p.chunkPuts.Add(1)
		p.mu.Lock()
		hold := p.next
		if hold != nil {
			p.next = nil
			p.active = hold
		}
		p.mu.Unlock()
		if hold == nil {
			return nil
		}
		event := p.record(faultEvent{Event: "chunk_put_response_held", Method: res.Request.Method, Path: res.Request.URL.Path, Status: res.StatusCode})
		hold.seen <- event
		select {
		case <-hold.release:
			p.record(faultEvent{Event: "chunk_put_response_released", Method: res.Request.Method, Path: res.Request.URL.Path})
			return nil
		case <-res.Request.Context().Done():
			p.record(faultEvent{Event: "chunk_put_client_disconnected", Method: res.Request.Method, Path: res.Request.URL.Path})
			_ = res.Body.Close()
			return errors.New("client disconnected during held response")
		}
	}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.metadataFailure.Load() && strings.Contains(r.URL.Path, "/meta/") && (r.Method == http.MethodPut || r.Method == http.MethodGet) {
			event := p.record(faultEvent{Event: "metadata_request_failed", Method: r.Method, Path: r.URL.Path, Status: http.StatusServiceUnavailable})
			select {
			case p.metadataSeen <- event:
			default:
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "<Error><Code>ServiceUnavailable</Code><Message>Injected metadata outage</Message></Error>")
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { p.Release(); p.server.Close(); p.eventMu.Lock(); _ = events.Close(); p.eventMu.Unlock() })
	return p
}
func (p *faultProxy) URL() string { return p.server.URL }
func (p *faultProxy) record(event faultEvent) faultEvent {
	event.Time = time.Now().UTC()
	p.eventMu.Lock()
	_ = json.NewEncoder(p.events).Encode(event)
	_ = p.events.Sync()
	p.eventMu.Unlock()
	return event
}
func (p *faultProxy) Record(event string) { p.record(faultEvent{Event: event}) }
func (p *faultProxy) HoldNextChunkResponse() <-chan faultEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	hold := &heldResponse{seen: make(chan faultEvent, 1), release: make(chan struct{})}
	p.next = hold
	return hold.seen
}
func (p *faultProxy) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, hold := range []*heldResponse{p.next, p.active} {
		if hold != nil {
			hold.once.Do(func() { close(hold.release) })
		}
	}
	p.next = nil
	p.active = nil
}
func (p *faultProxy) SetMetadataFailure(enabled bool) {
	p.metadataFailure.Store(enabled)
	if enabled {
		p.Record("metadata_failure_enabled")
	} else {
		p.Record("metadata_failure_disabled")
	}
}
func (p *faultProxy) MetadataFailureSeen() <-chan faultEvent { return p.metadataSeen }
