// Package fixture serves a real storage adapter through the SMB server for tests.
// It is separate from the raw client package to avoid cycles in server tests.
package fixture

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/djosh34/s3-smb/internal/smb/server"
)

// Server owns the listener and SMB server, not its storage runtime or clients.
// Use Start, then Close before releasing storage. Address is immutable; Close
// may be called repeatedly or concurrently.
type Server struct {
	server   *server.Server
	done     chan struct{}
	serveErr error
	address  string
}

// Start listens on 127.0.0.1:0 with the supplied real adapter. The listener is
// registered with the server before Start returns. Canceling ctx stops serving;
// Close still needs to wait for cleanup and report its errors.
func Start(ctx context.Context, options server.Options) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	smbServer, err := server.New(options)
	if err != nil {
		return nil, fmt.Errorf("fixture server: %w", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("fixture listener: %w", err)
	}
	ready := &readyListener{Listener: listener, ready: make(chan struct{})}
	fixture := &Server{server: smbServer, done: make(chan struct{}), address: listener.Addr().String()}
	go func() {
		fixture.serveErr = smbServer.Serve(ctx, ready)
		close(fixture.done)
	}()
	select {
	case <-ready.ready:
		return fixture, nil
	case <-fixture.done:
		return nil, errors.Join(errors.New("fixture stopped before accepting"), fixture.serveErr)
	}
}

type readyListener struct {
	net.Listener
	ready chan struct{}
	once  sync.Once
}

func (listener *readyListener) Accept() (net.Conn, error) {
	listener.once.Do(func() { close(listener.ready) })
	return listener.Listener.Accept()
}

// Address returns the loopback host:port.
func (fixture *Server) Address() string { return fixture.address }

// Close shuts down the server and waits for its serving goroutine. It returns
// shutdown and serving errors, including cancellation of Start's context.
// If ctx expires, cleanup continues; a later Close waits for its result.
func (fixture *Server) Close(ctx context.Context) error {
	shutdownErr := fixture.server.Shutdown(ctx)
	select {
	case <-fixture.done:
		return errors.Join(shutdownErr, fixture.serveErr)
	case <-ctx.Done():
		return errors.Join(shutdownErr, ctx.Err())
	}
}
