// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Only object paths and transport outcomes are recorded, never signed query
// strings, headers, credentials, request bodies or underlying error messages.
type event struct {
	Sequence       uint64    `json:"sequence"`
	Time           time.Time `json:"time"`
	Event          string    `json:"event"`
	RequestID      uint64    `json:"request_id,omitempty"`
	Method         string    `json:"method,omitempty"`
	Path           string    `json:"path,omitempty"`
	Key            string    `json:"key,omitempty"`
	UpstreamStatus int       `json:"upstream_status,omitempty"`
	Status         int       `json:"status,omitempty"`
	Bytes          int64     `json:"bytes,omitempty"`
}

type holdState struct {
	RequestID      uint64     `json:"request_id"`
	Path           string     `json:"path"`
	Key            string     `json:"key"`
	HeldAt         time.Time  `json:"held_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	UpstreamStatus int        `json:"upstream_status"`
	Pending        bool       `json:"pending"`
	Outcome        string     `json:"outcome"`
}

type proxyState struct {
	ObservedAt           time.Time  `json:"observed_at"`
	Armed                bool       `json:"armed"`
	PendingCount         int        `json:"pending_count"` // only the one held response, not all HTTP traffic
	Hold                 *holdState `json:"hold"`          // last hold, retained after disconnect/release
	ChunkGetSuccess      uint64     `json:"chunk_get_success"`
	ChunkGetSuccessBytes int64      `json:"chunk_get_success_bytes"`
	EvidenceOK           bool       `json:"evidence_ok"`
}

type requestInfo struct {
	id   uint64
	path string
	key  string
}
type requestKey struct{}

type proxy struct {
	forward     *httputil.ReverseProxy
	mu          sync.Mutex // state and ordered/synced evidence share one lock
	writer      io.Writer
	seq         uint64
	nextID      uint64
	state       proxyState
	releaseHold chan struct{}
	failed      chan struct{}
}

func newProxy(target *url.URL, writer io.Writer) *proxy {
	p := &proxy{writer: writer, failed: make(chan struct{}), state: proxyState{EvidenceOK: true}}
	p.forward = httputil.NewSingleHostReverseProxy(target)
	// Same boundary as test/e2e/fault_proxy_test.go. Preserve the incoming
	// signed Host; change only the dial destination, and never use env proxies.
	p.forward.Transport = &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConnsPerHost: 32, ResponseHeaderTimeout: 2 * time.Minute}
	p.forward.ErrorLog = log.New(io.Discard, "", 0)
	p.forward.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
		info := r.Context().Value(requestKey{}).(requestInfo)
		p.mu.Lock()
		p.record(event{Event: "upstream_error", RequestID: info.id, Method: r.Method, Path: info.path, Key: info.key})
		p.mu.Unlock()
		http.Error(w, "fixture upstream unavailable", http.StatusBadGateway)
	}
	p.forward.ModifyResponse = p.observeResponse
	return p
}

// record is called with mu held. Evidence failures permanently fail closed.
func (p *proxy) record(e event) bool {
	if !p.state.EvidenceOK {
		return false
	}
	p.seq++
	e.Sequence = p.seq
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	err := json.NewEncoder(p.writer).Encode(e)
	if err == nil {
		if syncer, ok := p.writer.(interface{ Sync() error }); ok {
			err = syncer.Sync()
		}
	}
	if err != nil {
		p.state.EvidenceOK = false
		close(p.failed)
		return false
	}
	return true
}

func objectKey(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

func (p *proxy) observeResponse(res *http.Response) error {
	r := res.Request
	info := r.Context().Value(requestKey{}).(requestInfo)
	p.mu.Lock()
	if !p.record(event{Event: "upstream_response", RequestID: info.id, Method: r.Method, Path: info.path, Key: info.key, UpstreamStatus: res.StatusCode}) {
		p.mu.Unlock()
		return errors.New("fixture evidence unavailable")
	}
	success := res.StatusCode >= 200 && res.StatusCode < 300
	if success && strings.Contains(info.path, "/meta/") {
		if !p.record(event{Event: "metadata_upstream_success", RequestID: info.id, Method: r.Method, Path: info.path, Key: info.key, UpstreamStatus: res.StatusCode}) {
			p.mu.Unlock()
			return errors.New("fixture evidence unavailable")
		}
	}
	if !p.state.Armed || !success || r.Method != http.MethodPut || !strings.Contains(info.path, "/chunks/") {
		p.mu.Unlock()
		return nil
	}
	p.state.Armed = false
	hold := &holdState{RequestID: info.id, Path: info.path, Key: info.key, HeldAt: time.Now().UTC(), UpstreamStatus: res.StatusCode, Pending: true, Outcome: "upstream_committed_response_pending"}
	p.state.Hold = hold
	p.state.PendingCount = 1
	release := make(chan struct{})
	p.releaseHold = release
	ok := p.record(event{Event: "chunk_put_response_held", Time: hold.HeldAt, RequestID: info.id, Method: r.Method, Path: info.path, Key: info.key, UpstreamStatus: res.StatusCode})
	p.mu.Unlock()
	if !ok {
		return errors.New("fixture evidence unavailable")
	}

	// The successful MinIO PUT already happened. Only its response to the
	// daemon is held: no claim is made that this object is uncommitted.
	select {
	case <-release:
		return nil
	case <-r.Context().Done():
		p.mu.Lock()
		if hold.Pending {
			now := time.Now().UTC()
			hold.Pending, hold.EndedAt, hold.Outcome = false, &now, "client_disconnected"
			p.state.PendingCount = 0
			p.record(event{Event: "chunk_put_client_disconnected", Time: now, RequestID: info.id, Method: r.Method, Path: info.path, Key: info.key})
		}
		p.mu.Unlock()
		res.Body.Close()
		return errors.New("client disconnected during held response")
	case <-p.failed:
		res.Body.Close()
		return errors.New("fixture evidence unavailable")
	}
}

type responseCounter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseCounter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseCounter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseCounter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.nextID++
	info := requestInfo{id: p.nextID, path: r.URL.Path, key: objectKey(r.URL.Path)}
	ok := p.record(event{Event: "request_started", RequestID: info.id, Method: r.Method, Path: info.path, Key: info.key})
	p.mu.Unlock()
	if !ok {
		http.Error(w, "fixture evidence unavailable", http.StatusServiceUnavailable)
		return
	}
	counter := &responseCounter{ResponseWriter: w}
	completed := false
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		name := "response_complete"
		if !completed || r.Context().Err() != nil {
			name = "response_interrupted"
		} else if counter.status >= 200 && counter.status < 300 && r.Method == http.MethodGet && strings.Contains(info.path, "/chunks/") {
			p.state.ChunkGetSuccess++
			p.state.ChunkGetSuccessBytes += counter.bytes
		}
		p.record(event{Event: name, RequestID: info.id, Method: r.Method, Path: info.path, Key: info.key, Status: counter.status, Bytes: counter.bytes})
	}()
	p.forward.ServeHTTP(counter, r.WithContext(context.WithValue(r.Context(), requestKey{}, info)))
	completed = true
}

func (p *proxy) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Armed = false
	if p.state.Hold != nil && p.state.Hold.Pending {
		h := p.state.Hold
		now := time.Now().UTC()
		h.Pending, h.EndedAt, h.Outcome = false, &now, "released"
		p.state.PendingCount = 0
		p.record(event{Event: "chunk_put_response_released", Time: now, RequestID: h.RequestID, Method: http.MethodPut, Path: h.Path, Key: h.Key})
	}
	if p.releaseHold != nil {
		close(p.releaseHold)
		p.releaseHold = nil
	}
	p.record(event{Event: "hold_disarmed"})
}

func (p *proxy) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) { p.writeState(w) })
	mux.HandleFunc("POST /hold", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		if p.state.Armed || (p.state.Hold != nil && p.state.Hold.Pending) {
			p.mu.Unlock()
			http.Error(w, "hold already armed or pending", http.StatusConflict)
			return
		}
		p.state.Armed = true
		p.record(event{Event: "hold_armed"})
		p.mu.Unlock()
		p.writeState(w)
	})
	mux.HandleFunc("POST /release", func(w http.ResponseWriter, _ *http.Request) { p.release(); p.writeState(w) })
	return mux
}

func (p *proxy) writeState(w http.ResponseWriter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.ObservedAt = time.Now().UTC()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !p.state.EvidenceOK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(p.state)
}
