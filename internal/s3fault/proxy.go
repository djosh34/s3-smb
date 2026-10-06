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
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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

// Fault applies to requests with a matching Method (empty matches all) until
// SetFault replaces it. HeaderDelay holds the response headers. Status, if
// set, answers with an S3 XML error after the delay without forwarding; Code
// is its S3 error code, ServiceUnavailable when empty. Cut forwards the
// request, then sends the headers and half the body of the response and
// closes the connection, or closes it at once when the body is empty.
type Fault struct {
	Method      string
	Code        string
	HeaderDelay time.Duration
	Status      int
	Cut         bool
}

// Mix draws a fault for each request on its own, so requests in flight end
// out of order and some fail while others succeed. Fail is the share that
// get an error, Cut the share forwarded and then cut, which S3 may have
// accepted, and every request waits up to MaxDelay first.
type Mix struct {
	Fail, Cut float64
	MaxDelay  time.Duration
}

type heldResponse struct {
	seen    chan Event
	release chan struct{}
	once    sync.Once
}

// Proxy owns its loopback HTTP server and upstream transport. Methods are safe
// for concurrent use. Construct it with New and call Close to stop it, which
// also ends delays and holds.
type Proxy struct {
	server      *http.Server
	target      *url.URL
	transport   *http.Transport
	next        *heldResponse
	active      *heldResponse
	rng         *rand.Rand
	outageSeen  chan Event
	done        chan struct{}
	served      chan error
	closeErr    error
	address     string
	fault       Fault
	mix         Mix
	chunkPuts   atomic.Int64
	outageUntil atomic.Int64
	mu          sync.Mutex
	closeOnce   sync.Once
}

// New starts a proxy for an http:// test backend.
func New(ctx context.Context, upstream string) (*Proxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse fault proxy upstream: %w", err)
	}
	if target.Scheme != "http" || target.Host == "" {
		return nil, errors.New("fault proxy requires an http:// test backend")
	}
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for fault proxy: %w", err)
	}
	p := &Proxy{
		rng:    rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), //nolint:gosec // Test faults, not secrets.
		target: target, transport: &http.Transport{Proxy: nil},
		outageSeen: make(chan Event, 32),
		done:       make(chan struct{}), served: make(chan error, 1),
		address: "http://" + listener.Addr().String(),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Only the dial address changes. Host remains the one used for signing.
	proxy.Transport = p.transport
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeError(w, http.StatusBadGateway, "")
	}
	p.server = &http.Server{Handler: p.handler(proxy), ReadHeaderTimeout: 5 * time.Second}
	go func() { p.served <- p.server.Serve(listener) }()
	return p, nil
}

// URL returns the proxy endpoint.
func (p *Proxy) URL() string { return p.address }

// Close stops the proxy, including held or delayed responses. Repeated calls
// return the same result.
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

// SetFault replaces the current fault for later requests. A zero Fault
// restores normal responses. Invalid values leave the fault unchanged.
func (p *Proxy) SetFault(fault Fault) error {
	if fault.HeaderDelay < 0 || (fault.Status != 0 && (fault.Status < 400 || fault.Status > 599)) {
		return errors.New("invalid S3 fault delay or error status")
	}
	if fault.Cut && fault.Status != 0 {
		return errors.New("an S3 fault cannot both cut and answer with an error")
	}
	if fault.Code != "" && fault.Status == 0 {
		return errors.New("an S3 error code needs an error status")
	}
	p.mu.Lock()
	p.fault = fault
	p.mu.Unlock()
	return nil
}

// SetMix replaces the per-request mix. It applies only while no Fault is set.
// A zero Mix ends it.
func (p *Proxy) SetMix(mix Mix) error {
	if mix.Fail < 0 || mix.Cut < 0 || mix.Fail+mix.Cut > 1 || mix.MaxDelay < 0 {
		return errors.New("invalid S3 fault mix")
	}
	p.mu.Lock()
	p.mix = mix
	p.mu.Unlock()
	return nil
}

// current returns the fault for one request: the set Fault, or one drawn
// from the mix.
func (p *Proxy) current() Fault {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fault != (Fault{}) || p.mix == (Mix{}) {
		return p.fault
	}
	fault := Fault{}
	if p.mix.MaxDelay > 0 {
		fault.HeaderDelay = time.Duration(p.rng.Int64N(int64(p.mix.MaxDelay)))
	}
	switch draw := p.rng.Float64(); {
	case draw < p.mix.Fail:
		fault.Status = http.StatusServiceUnavailable
	case draw < p.mix.Fail+p.mix.Cut:
		fault.Cut = true
	}
	return fault
}

func (p *Proxy) handler(proxy *httputil.ReverseProxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().UnixNano() < p.outageUntil.Load() {
			if strings.Contains(r.URL.Path, "/chunks/") {
				notify(p.outageSeen, r)
			}
			writeError(w, http.StatusServiceUnavailable, "")
			return
		}
		fault := p.current()
		if fault.Method != "" && fault.Method != r.Method {
			fault = Fault{}
		}
		if fault.Status != 0 {
			if err := p.wait(r.Context(), fault.HeaderDelay); err != nil {
				writeError(w, http.StatusBadGateway, "")
				return
			}
			writeError(w, fault.Status, fault.Code)
			return
		}
		if fault.Cut {
			if err := p.cut(w, r, fault.HeaderDelay); err != nil {
				// The caller sees the cut either way.
				log.Printf("s3 fault proxy: cut response: %v", err)
			}
			return
		}
		// A per-request copy avoids sharing ModifyResponse state across requests.
		requestProxy := *proxy
		ctx := r.Context()
		requestProxy.ModifyResponse = func(res *http.Response) error {
			if err := p.holdChunkResponse(ctx, res); err != nil {
				return err
			}
			return p.wait(ctx, fault.HeaderDelay)
		}
		requestProxy.ServeHTTP(w, r)
	})
}

// writeError answers with an S3 XML error with code, ServiceUnavailable when
// empty.
func writeError(w http.ResponseWriter, status int, code string) {
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
		// Encoding only strings fails only when the client stops reading.
		log.Printf("s3 fault proxy: write injected error: %v", err)
	}
}

// cut forwards r and lets half of the response through after delay. It always
// ends by closing the connection.
func (p *Proxy) cut(w http.ResponseWriter, r *http.Request, delay time.Duration) error {
	err := p.forwardHalf(w, r, delay)
	conn, _, hijackErr := http.NewResponseController(w).Hijack()
	if hijackErr != nil {
		return errors.Join(err, hijackErr)
	}
	return errors.Join(err, conn.Close())
}

func (p *Proxy) forwardHalf(w http.ResponseWriter, r *http.Request, delay time.Duration) error {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme, out.URL.Host = p.target.Scheme, p.target.Host
	res, err := p.transport.RoundTrip(out)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(res.Body)
	if err = errors.Join(err, res.Body.Close(), p.wait(r.Context(), delay)); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	maps.Copy(w.Header(), res.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(res.StatusCode)
	if _, err = w.Write(body[:len(body)/2]); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

// notify never blocks, so observation cannot stall an S3 caller.
func notify(events chan Event, r *http.Request) {
	select {
	case events <- Event{Method: r.Method, Path: r.URL.Path, Status: http.StatusServiceUnavailable}:
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

// HoldNextChunkResponse holds the next successful chunk PUT before sending its
// headers. The event proves that S3 accepted the data. Only one hold can be
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

// FailS3For answers every S3 request with 503, without forwarding, until
// duration has passed. It returns the start time. Calling it again replaces
// the deadline.
func (p *Proxy) FailS3For(duration time.Duration) time.Time {
	start := time.Now()
	p.outageUntil.Store(start.Add(duration).UnixNano())
	return start
}

// RestoreS3 ends an outage immediately.
func (p *Proxy) RestoreS3() { p.outageUntil.Store(0) }

// OutageSeen reports chunk requests rejected by an outage. Events are dropped
// when its buffer is full.
func (p *Proxy) OutageSeen() <-chan Event { return p.outageSeen }

// ChunkPuts returns the number of successful chunk PUT responses.
func (p *Proxy) ChunkPuts() int64 { return p.chunkPuts.Load() }
