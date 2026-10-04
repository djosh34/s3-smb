// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// faultEvent holds no headers or bodies, because signed requests contain
// credentials.
type faultEvent struct {
	Method string
	Path   string
	Status int
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
	metadataFailure atomic.Bool
	metadataSeen    chan faultEvent
	chunkPuts       atomic.Int64 // Successful real data PUTs, no headers/bodies retained.
	outageUntil     atomic.Int64
	outageSeen      chan faultEvent
}

func newFaultProxy(t *testing.T, upstream string) *faultProxy {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" || target.Scheme != "http" {
		t.Fatal("fault proxy requires explicit disposable HTTP MinIO")
	}
	p := &faultProxy{metadataSeen: make(chan faultEvent, 32), outageSeen: make(chan faultEvent, 32)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// NewSingleHostReverseProxy preserves the signed request Host, including the
	// loopback proxy port; only the upstream dial address changes.
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
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
		hold.seen <- faultEvent{Method: res.Request.Method, Path: res.Request.URL.Path, Status: res.StatusCode}
		select {
		case <-hold.release:
			return nil
		case <-res.Request.Context().Done():
			_ = res.Body.Close()
			return errors.New("client disconnected during held response")
		}
	}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().UnixNano() < p.outageUntil.Load() {
			if strings.Contains(r.URL.Path, "/chunks/") {
				select {
				case p.outageSeen <- faultEvent{Method: r.Method, Path: r.URL.Path, Status: http.StatusServiceUnavailable}:
				default:
				}
			}
			http.Error(w, "<Error><Code>ServiceUnavailable</Code><Message>Injected S3 outage</Message></Error>", http.StatusServiceUnavailable)
			return
		}
		if p.metadataFailure.Load() && strings.Contains(r.URL.Path, "/meta/") && (r.Method == http.MethodPut || r.Method == http.MethodGet) {
			select {
			case p.metadataSeen <- faultEvent{Method: r.Method, Path: r.URL.Path, Status: http.StatusServiceUnavailable}:
			default:
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "<Error><Code>ServiceUnavailable</Code><Message>Injected metadata outage</Message></Error>")
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { p.Release(); p.server.Close() })
	return p
}
func (p *faultProxy) URL() string { return p.server.URL }
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

// FailS3For rejects every S3 request until the deadline, without forwarding it.
func (p *faultProxy) FailS3For(duration time.Duration) time.Time {
	start := time.Now()
	p.outageUntil.Store(start.Add(duration).UnixNano())
	return start
}

func (p *faultProxy) RestoreS3() { p.outageUntil.Store(0) }

func (p *faultProxy) SetMetadataFailure(enabled bool)        { p.metadataFailure.Store(enabled) }
func (p *faultProxy) MetadataFailureSeen() <-chan faultEvent { return p.metadataSeen }
