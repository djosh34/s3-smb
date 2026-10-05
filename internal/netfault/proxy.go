// SPDX-License-Identifier: AGPL-3.0-only

// Package netfault provides a TCP proxy for tests that cuts and refuses
// connections on demand.
package netfault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Proxy forwards connections from a loopback listener to one upstream address.
// Methods are safe for concurrent use. Construct it with New.
type Proxy struct {
	ctx      context.Context
	listener net.Listener
	closeErr error
	cancel   context.CancelFunc
	conns    map[net.Conn]struct{}
	upstream string
	workers  sync.WaitGroup
	mu       sync.Mutex
	drop     bool
	closed   bool
}

// New starts a proxy on 127.0.0.1 that forwards to upstream (host:port).
// Canceling ctx stops new dials; call Close to stop the proxy.
func New(ctx context.Context, upstream string) (*Proxy, error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for network fault proxy: %w", err)
	}
	lifetime, cancel := context.WithCancel(ctx)
	p := &Proxy{
		ctx: lifetime, cancel: cancel, listener: listener,
		upstream: upstream, conns: make(map[net.Conn]struct{}),
	}
	p.workers.Add(1)
	go p.accept()
	return p, nil
}

// Address returns the proxy's loopback host:port.
func (p *Proxy) Address() string { return p.listener.Addr().String() }

// Drop cuts all current connections and refuses new ones until Restore.
func (p *Proxy) Drop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drop = true
	return p.cut()
}

// Restore forwards new connections again after Drop.
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drop = false
}

// Close stops the proxy, closes all connections and waits for its workers.
// It returns any error from closing the listener or connections.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.cancel()
		p.closeErr = errors.Join(p.closeErr, ignoreClosed(p.listener.Close()), p.cut())
	}
	p.mu.Unlock()
	p.workers.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeErr
}

// cut runs under mu.
func (p *Proxy) cut() error {
	var err error
	for conn := range p.conns {
		err = errors.Join(err, ignoreClosed(conn.Close()))
		delete(p.conns, conn)
	}
	return err
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
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
			return
		}
		p.mu.Lock()
		if p.closed || p.drop {
			p.closeErr = errors.Join(p.closeErr, ignoreClosed(client.Close()))
			p.mu.Unlock()
			continue
		}
		p.conns[client] = struct{}{}
		p.workers.Add(1)
		p.mu.Unlock()
		go p.forward(client)
	}
}

func (p *Proxy) forward(client net.Conn) {
	defer p.workers.Done()
	var dialer net.Dialer
	upstream, err := dialer.DialContext(p.ctx, "tcp", p.upstream)
	p.mu.Lock()
	if _, open := p.conns[client]; err != nil || !open {
		// Closing the client shows the caller that the peer is unavailable.
		p.closeErr = errors.Join(p.closeErr, p.release(client))
		if err == nil {
			p.closeErr = errors.Join(p.closeErr, ignoreClosed(upstream.Close()))
		}
		p.mu.Unlock()
		return
	}
	p.conns[upstream] = struct{}{}
	p.mu.Unlock()

	// The first direction to end, by EOF, error or cut, ends the link. Copy
	// errors are the expected end of test traffic, not proxy failures.
	ended := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(upstream, client)
		ended <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(client, upstream)
		ended <- copyErr
	}()
	<-ended
	p.mu.Lock()
	p.closeErr = errors.Join(p.closeErr, p.release(client), p.release(upstream))
	p.mu.Unlock()
	<-ended
}

// release closes conn unless a cut already did; it runs under mu.
func (p *Proxy) release(conn net.Conn) error {
	if _, open := p.conns[conn]; !open {
		return nil
	}
	delete(p.conns, conn)
	return ignoreClosed(conn.Close())
}
