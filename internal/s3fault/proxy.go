// SPDX-License-Identifier: AGPL-3.0-only

// Package s3fault provides an HTTP fault proxy for disposable S3 test backends.
// It preserves the signed request Host and never records headers or bodies.
package s3fault

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event describes an observed request without credentials or payloads.
type Event struct {
	Method string
	Path   string
	Status int
}

// Fault applies to matching requests until SetFault replaces it. Empty Method
// and PathContains match every request. HeaderDelay precedes response headers;
// BodyDelay applies before every body read, not just the first byte. Total delay
// grows with body size and depends on the reverse proxy's read buffer (currently
// 32 KiB). Status injects an S3 XML error without forwarding; Code defaults to
// ServiceUnavailable. For throttling, use status 503 and code SlowDown. CutBody
// sends at most CutAfter bytes and aborts a shortened response, including chunked
// responses. It leaves the original Content-Length intact.
// A request snapshots its fault, so replacement affects only later requests.
type Fault struct {
	Method       string
	PathContains string
	Code         string
	HeaderDelay  time.Duration
	BodyDelay    time.Duration
	Status       int
	CutAfter     int64
	CutBody      bool
}

type heldResponse struct {
	seen    chan Event
	release chan struct{}
	once    sync.Once
}

// Proxy owns its loopback HTTP server and upstream transport. Methods are safe
// for concurrent use. Call Close to cancel delays, release holds and stop it.
// Construct it with New; the zero value is not usable.
type Proxy struct {
	server       *http.Server
	transport    *http.Transport
	next         *heldResponse
	active       *heldResponse
	metadataSeen chan Event
	outageSeen   chan Event
	done         chan struct{}
	served       chan error
	closeErr     error
	address      string
	fault        Fault
	chunkPuts    atomic.Int64
	outageUntil  atomic.Int64
	mu           sync.Mutex
	closeOnce    sync.Once
	metadataFail atomic.Bool
}

// New starts a proxy for an explicit HTTP backend. It is for test buckets only,
// not production endpoints. Construction and listener errors are returned.
func New(ctx context.Context, upstream string) (*Proxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse fault proxy upstream: %w", err)
	}
	if target.Host == "" || target.Scheme != "http" || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("fault proxy requires an explicit disposable HTTP backend")
	}
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for fault proxy: %w", err)
	}
	p := &Proxy{
		transport:    &http.Transport{Proxy: nil},
		metadataSeen: make(chan Event, 32), outageSeen: make(chan Event, 32),
		done: make(chan struct{}), served: make(chan error, 1),
		address: "http://" + listener.Addr().String(),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Only the dial address changes. Host remains the one used for signing.
	proxy.Transport = p.transport
	proxy.FlushInterval = -1 // Send headers before applying body delays.
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		p.writeError(w, http.StatusBadGateway, "ServiceUnavailable")
	}
	p.server = &http.Server{
		Handler: p.handler(proxy), ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { p.served <- p.server.Serve(listener) }()
	return p, nil
}

// URL returns the proxy endpoint.
func (p *Proxy) URL() string { return p.address }

// Close stops the proxy, including held or delayed responses. Repeated calls
// return the same result, including any server shutdown error.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		close(p.done)
		p.Release()
		p.closeErr = p.server.Close()
		if err := <-p.served; !errors.Is(err, http.ErrServerClosed) {
			p.closeErr = errors.Join(p.closeErr, err)
		}
		p.transport.CloseIdleConnections()
	})
	return p.closeErr
}

// SetFault replaces the current fault. A zero Fault restores normal responses.
// Invalid values leave the previous fault unchanged.
func (p *Proxy) SetFault(fault Fault) error {
	if fault.HeaderDelay < 0 || fault.BodyDelay < 0 || fault.CutAfter < 0 || (fault.Status != 0 && (fault.Status < 400 || fault.Status > 599)) {
		return errors.New("invalid S3 fault delay, cut point or error status")
	}
	p.mu.Lock()
	p.fault = fault
	p.mu.Unlock()
	return nil
}

func (p *Proxy) handler(proxy *httputil.ReverseProxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().UnixNano() < p.outageUntil.Load() {
			p.observeOutage(r)
			p.writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
			return
		}
		if p.metadataFail.Load() && strings.Contains(r.URL.Path, "/meta/") && (r.Method == http.MethodPut || r.Method == http.MethodGet) {
			notify(p.metadataSeen, r, http.StatusServiceUnavailable)
			p.writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
			return
		}
		p.mu.Lock()
		fault := p.fault
		p.mu.Unlock()
		if (fault.Method != "" && fault.Method != r.Method) || !strings.Contains(r.URL.Path, fault.PathContains) {
			fault = Fault{}
		}
		if fault.Status != 0 {
			if err := p.wait(r.Context(), fault.HeaderDelay); err != nil {
				p.writeError(w, http.StatusBadGateway, "ServiceUnavailable")
				return
			}
			p.writeError(w, fault.Status, fault.Code)
			return
		}
		// A per-request copy avoids sharing ModifyResponse state across requests.
		requestProxy := *proxy
		ctx := r.Context()
		requestProxy.ModifyResponse = func(res *http.Response) error {
			if err := p.holdChunkResponse(ctx, res); err != nil {
				return err
			}
			if err := p.wait(ctx, fault.HeaderDelay); err != nil {
				return err
			}
			if fault.BodyDelay != 0 || fault.CutBody {
				res.Body = &faultBody{ReadCloser: res.Body, proxy: p, ctx: ctx, delay: fault.BodyDelay, remaining: fault.CutAfter, cut: fault.CutBody}
			}
			return nil
		}
		requestProxy.ServeHTTP(w, r)
	})
}

func (p *Proxy) observeOutage(r *http.Request) {
	if strings.Contains(r.URL.Path, "/chunks/") {
		notify(p.outageSeen, r, http.StatusServiceUnavailable)
	}
	if strings.Contains(r.URL.Path, "/meta/") {
		notify(p.metadataSeen, r, http.StatusServiceUnavailable)
	}
}

func (p *Proxy) writeError(w http.ResponseWriter, status int, code string) {
	if code == "" {
		code = "ServiceUnavailable"
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	body := struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: "Injected S3 fault"}
	if err := xml.NewEncoder(w).Encode(body); err != nil {
		// Encoding only strings cannot fail except when the client stops reading.
		log.Printf("s3 fault proxy: write injected error: %v", err)
	}
}

func notify(events chan Event, r *http.Request, status int) {
	select {
	case events <- Event{Method: r.Method, Path: r.URL.Path, Status: status}:
	default:
	}
}

func (p *Proxy) wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("fault proxy closed")
	case <-timer.C:
		return nil
	}
}

func (p *Proxy) holdChunkResponse(ctx context.Context, res *http.Response) error {
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
	hold.seen <- Event{Method: res.Request.Method, Path: res.Request.URL.Path, Status: res.StatusCode}
	select {
	case <-hold.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("fault proxy closed")
	}
}

// HoldNextChunkResponse holds the next successful real chunk PUT before sending
// its headers. The event proves that S3 accepted the data. Only one hold can be
// armed at a time; call Release before arming another.
func (p *Proxy) HoldNextChunkResponse() (<-chan Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next != nil || p.active != nil {
		return nil, errors.New("chunk response hold already armed")
	}
	hold := &heldResponse{seen: make(chan Event, 1), release: make(chan struct{})}
	p.next = hold
	return hold.seen, nil
}

// Release clears and releases any armed or active chunk response hold.
func (p *Proxy) Release() {
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

// FailS3For rejects every S3 request until start+duration, without forwarding.
// It returns start so tests can measure the outage. A nonpositive duration ends
// the outage immediately. Calling it again replaces the deadline.
func (p *Proxy) FailS3For(duration time.Duration) time.Time {
	start := time.Now()
	p.outageUntil.Store(start.Add(duration).UnixNano())
	return start
}

// RestoreS3 ends an outage immediately.
func (p *Proxy) RestoreS3() { p.outageUntil.Store(0) }

// OutageSeen reports rejected chunk requests. Events are dropped if its buffer
// fills, so observation never stalls an S3 caller.
func (p *Proxy) OutageSeen() <-chan Event { return p.outageSeen }

// SetMetadataFailure rejects metadata GETs and PUTs while enabled.
func (p *Proxy) SetMetadataFailure(enabled bool) { p.metadataFail.Store(enabled) }

// MetadataFailureSeen reports metadata requests rejected by a metadata failure
// or a timed S3 outage, with bounded buffering.
func (p *Proxy) MetadataFailureSeen() <-chan Event { return p.metadataSeen }

// ChunkPuts returns the number of successful real chunk PUT responses.
func (p *Proxy) ChunkPuts() int64 { return p.chunkPuts.Load() }

type faultBody struct {
	io.ReadCloser
	proxy     *Proxy
	ctx       context.Context
	delay     time.Duration
	remaining int64
	cut       bool
}

func (body *faultBody) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if err := body.proxy.wait(body.ctx, body.delay); err != nil {
		return 0, err
	}
	if body.cut {
		if body.remaining == 0 {
			// Distinguish a real end from a cut, even without Content-Length.
			// A non-EOF error makes ReverseProxy abort instead of writing the
			// terminating chunk that would turn the cut into a successful read.
			var probe [1]byte
			n, err := body.ReadCloser.Read(probe[:])
			if n != 0 {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if int64(len(dst)) > body.remaining {
			dst = dst[:body.remaining]
		}
	}
	n, err := body.ReadCloser.Read(dst)
	if body.cut {
		body.remaining -= int64(n)
	}
	return n, err
}
