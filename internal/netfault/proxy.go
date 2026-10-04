// SPDX-License-Identifier: AGPL-3.0-only

// Package netfault provides a TCP fault proxy for disposable test peers.
// It needs no packet filters and never records traffic contents.
package netfault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

var errSelfTarget = errors.New("network fault proxy cannot target its own address")

// Fault applies in both directions until SetFault replaces it. Delay precedes
// each forwarded buffer (at most 32 KiB), not each packet. Stall pauses forwarding
// without discarding buffered data. Drop closes current connections and rejects
// new ones without dialing the peer. A zero Fault restores normal forwarding.
// Changes wake pending delays and stalls; bytes already written cannot be recalled.
type Fault struct {
	Delay time.Duration
	Stall bool
	Drop  bool
}

// Step replaces the fault After elapsed time from Schedule. Cut also closes
// current connections at that step, without rejecting later ones unless Drop is
// set. Steps at the same time run in slice order.
type Step struct {
	After time.Duration
	Fault Fault
	Cut   bool
}

// Proxy owns a loopback listener and all accepted and upstream connections.
// Methods are safe for concurrent use. Construct it with New; the zero value is
// not usable. Cancel the construction context or call Close to stop all work.
type Proxy struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listener  net.Listener
	links     map[*link]struct{}
	changed   chan struct{}
	closeErr  error
	address   string
	upstreams []string
	fault     Fault
	mu        sync.Mutex
	workers   sync.WaitGroup
	closed    bool
	scheduled bool
}

type link struct {
	ctx      context.Context
	cancel   context.CancelFunc
	client   net.Conn
	upstream net.Conn
	closed   bool
}

// New starts a proxy for an explicit TCP host:port. The listener binds only to
// 127.0.0.1. It resolves the upstream once and rejects its own port when any
// target address is loopback or unspecified. Each accepted connection
// tries the resolved addresses in order; later DNS changes cannot bypass the check.
func New(ctx context.Context, upstream string) (*Proxy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for network fault proxy: %w", err)
	}
	return start(ctx, upstream, listener)
}

// start owns the listener, including on failure. Keeping listener creation
// separate lets tests force a collision with the upstream address.
func start(ctx context.Context, upstream string, listener net.Listener) (*Proxy, error) {
	addresses, port, err := resolvePeer(ctx, upstream)
	if err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	local, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return nil, errors.Join(errors.New("network fault listener requires a TCP address"), listener.Close())
	}
	if int(port) == local.Port {
		for _, address := range addresses {
			if address.IP.IsLoopback() || address.IP.IsUnspecified() {
				return nil, errors.Join(errSelfTarget, listener.Close())
			}
		}
	}
	targets := make([]string, len(addresses))
	for i, address := range addresses {
		targets[i] = net.JoinHostPort(address.String(), strconv.Itoa(int(port)))
	}
	lifetime, cancel := context.WithCancel(ctx)
	p := &Proxy{
		ctx: lifetime, cancel: cancel, listener: listener,
		links: make(map[*link]struct{}), changed: make(chan struct{}),
		upstreams: targets, address: listener.Addr().String(),
	}
	p.workers.Add(1)
	go p.accept()
	context.AfterFunc(lifetime, p.stop)
	return p, nil
}

func resolvePeer(ctx context.Context, upstream string) ([]net.IPAddr, uint16, error) {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil {
		return nil, 0, fmt.Errorf("parse network fault peer: %w", err)
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || host == "" || number == 0 {
		return nil, 0, errors.New("network fault peer requires a host and port in 1..65535")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve network fault peer: %w", err)
	}
	if len(addresses) == 0 {
		return nil, 0, errors.New("network fault peer has no addresses")
	}
	return addresses, uint16(number), nil
}

// Address returns the proxy's loopback host:port.
func (p *Proxy) Address() string { return p.address }

// Close stops accepting, cancels schedules and waits for forwarding to finish.
// Repeated calls return the same listener and connection cleanup errors. Peer
// disconnects, dial failures and injected faults terminate their connections;
// they are expected test traffic, not shutdown errors.
func (p *Proxy) Close() error {
	p.stop()
	p.workers.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeErr
}

func (p *Proxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.cancel()
	p.recordClose(p.listener.Close())
	for connection := range p.links {
		p.recordClose(p.closeLink(connection))
	}
}

func (p *Proxy) recordClose(err error) {
	if err != nil && !errors.Is(err, net.ErrClosed) {
		p.closeErr = errors.Join(p.closeErr, err)
	}
}

// closeLink runs under mu, including while an upstream dial is pending.
func (p *Proxy) closeLink(connection *link) error {
	if connection.closed {
		return nil
	}
	connection.closed = true
	connection.cancel()
	err := closeConn(connection.client)
	if connection.upstream != nil {
		err = errors.Join(err, closeConn(connection.upstream))
	}
	return err
}

func closeConn(conn net.Conn) error {
	err := conn.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// SetFault replaces the fault for current and future connections. Invalid
// values leave it unchanged. Cleanup errors concern only connections closed by
// this call; the fault still takes effect. Close reports all collected errors.
// Manual commands do not cancel an active schedule.
func (p *Proxy) SetFault(fault Fault) error {
	if fault.Delay < 0 {
		return errors.New("negative network fault delay")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apply(fault, false)
}

// Cut closes all current connections, including pending upstream dials. Later
// connections remain allowed unless the current fault has Drop set. It reports
// only this cut's cleanup errors; Close reports all collected errors.
func (p *Proxy) Cut() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apply(p.fault, true)
}

func (p *Proxy) apply(fault Fault, cut bool) error {
	if p.closed {
		return net.ErrClosed
	}
	p.fault = fault
	close(p.changed)
	p.changed = make(chan struct{})
	var err error
	if cut || fault.Drop {
		for connection := range p.links {
			err = errors.Join(err, p.closeLink(connection))
		}
	}
	p.recordClose(err)
	return err
}

func (p *Proxy) accept() {
	defer p.workers.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			p.mu.Lock()
			if !p.closed {
				p.closeErr = errors.Join(p.closeErr, fmt.Errorf("accept network fault client: %w", err))
			}
			p.mu.Unlock()
			p.stop()
			return
		}
		p.mu.Lock()
		if p.closed || p.fault.Drop {
			p.recordClose(client.Close())
			p.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(p.ctx)
		connection := &link{ctx: ctx, cancel: cancel, client: client}
		p.links[connection] = struct{}{}
		p.workers.Add(1)
		p.mu.Unlock()
		go p.forward(connection)
	}
}

func (p *Proxy) forward(connection *link) {
	defer p.workers.Done()
	defer func() {
		p.mu.Lock()
		p.recordClose(p.closeLink(connection))
		delete(p.links, connection)
		p.mu.Unlock()
	}()
	upstream, err := p.dialPeer(connection.ctx)
	if err != nil {
		// Closing the client exposes an unavailable peer to its caller.
		return
	}
	p.mu.Lock()
	if connection.closed {
		p.recordClose(upstream.Close())
		p.mu.Unlock()
		return
	}
	connection.upstream = upstream
	p.mu.Unlock()
	results := make(chan error, 2)
	go func() { results <- p.relay(connection.ctx, connection.client, upstream) }()
	go func() { results <- p.relay(connection.ctx, upstream, connection.client) }()
	if err := <-results; err != nil {
		// A failed direction must unblock the other direction's read or write.
		p.mu.Lock()
		p.recordClose(p.closeLink(connection))
		p.mu.Unlock()
	}
	// The deferred cleanup handles completion or failure in the second direction.
	<-results
}

func (p *Proxy) dialPeer(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	var dialErr error
	for _, address := range p.upstreams {
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err == nil {
			return conn, nil
		}
		dialErr = errors.Join(dialErr, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, dialErr
}

func (p *Proxy) relay(ctx context.Context, src, dst net.Conn) error {
	buffer := make([]byte, 32*1024)
	for {
		if err := p.wait(ctx, false); err != nil {
			return err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			if err := p.wait(ctx, true); err != nil {
				return err
			}
			written, err := dst.Write(buffer[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			// Preserve TCP half-close so a peer can answer after request EOF.
			half, ok := dst.(interface{ CloseWrite() error })
			if !ok {
				return errors.New("network fault peer cannot half-close")
			}
			return half.CloseWrite()
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (p *Proxy) wait(ctx context.Context, delay bool) error {
	start := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		fault, changed := p.fault, p.changed
		p.mu.Unlock()
		if fault.Stall {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
				continue
			}
		}
		remaining := time.Until(start.Add(fault.Delay))
		if !delay || remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
