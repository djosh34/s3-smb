package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/djosh34/s3-smb/internal/smb/auth"
)

// New validates the supplied modules and server identity. It does not own storage.
func New(options Options) (*Server, error) {
	if options.Storage == nil || options.State == nil || options.Logger == nil || options.Now == nil {
		return nil, errors.New("server requires storage, state, logger and clock")
	}
	if options.Encryption != RequireEncryption && options.Encryption != AllowPlaintext {
		return nil, errors.New("invalid encryption policy")
	}
	if options.ServerGUID == [16]byte{} {
		return nil, errors.New("server GUID is zero")
	}
	if options.ShareName == "" || !utf8.ValidString(options.ShareName) || strings.ContainsAny(options.ShareName, "\\/:\x00") || strings.EqualFold(options.ShareName, "IPC$") {
		return nil, errors.New("invalid disk share name")
	}
	if _, err := auth.NewAcceptor(auth.Options{Account: options.Account, ServerName: options.ServerName, Now: options.Now}); err != nil {
		return nil, fmt.Errorf("server account: %w", err)
	}
	return &Server{
		options: options, handlers: commandHandlers(),
		connections: make(map[*connection]struct{}), listeners: make(map[*ownedListener]struct{}), shutdownDone: make(chan struct{}),
	}, nil
}

type ownedListener struct {
	net.Listener
	err  error
	once sync.Once
}

func (listener *ownedListener) close() error {
	listener.once.Do(func() { listener.err = listener.Close() })
	return listener.err
}

// Serve owns listener. Cancellation or an accept error shuts down the server.
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("nil listener")
	}
	owned := &ownedListener{Listener: listener}
	server.mu.Lock()
	if server.stopping {
		server.mu.Unlock()
		return errors.Join(net.ErrClosed, owned.close())
	}
	server.listeners[owned] = struct{}{}
	server.mu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		if err := owned.close(); err != nil {
			server.options.Logger.Error("close listener", "error", err)
		}
	})
	defer stop()
	var acceptErr error
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				acceptErr = err
			}
			break
		}
		// Registration happens before starting the goroutine, so Shutdown cannot
		// finish while an accepted connection is still waiting to be registered.
		connCtx, cancel := context.WithCancel(ctx)
		connection, err := server.addConnection(connCtx, cancel, conn)
		if err != nil {
			acceptErr = err
			break
		}
		go func() {
			if err := server.runConnection(connCtx, connection); err != nil {
				server.options.Logger.Debug("connection ended", "error", err)
			}
		}()
	}
	return errors.Join(acceptErr, ctx.Err(), server.Shutdown(context.WithoutCancel(ctx)))
}

// ServeConn owns conn and serves framed requests until EOF, cancellation or error.
func (server *Server) ServeConn(ctx context.Context, conn net.Conn) error {
	if conn == nil {
		return errors.New("nil connection")
	}
	ctx, cancel := context.WithCancel(ctx)
	connection, err := server.addConnection(ctx, cancel, conn)
	if err != nil {
		return err
	}
	return server.runConnection(ctx, connection)
}

func (server *Server) addConnection(ctx context.Context, cancel context.CancelFunc, conn net.Conn) (*connection, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.stopping {
		cancel()
		return nil, errors.Join(net.ErrClosed, conn.Close())
	}
	connection := newConnection(ctx, cancel, server, conn)
	server.connections[connection] = struct{}{}
	server.workers.Add(1)
	return connection, nil
}

func (server *Server) runConnection(ctx context.Context, connection *connection) error {
	defer server.workers.Done()
	err := connection.serve(ctx)
	server.mu.Lock()
	delete(server.connections, connection)
	server.mu.Unlock()
	return err
}

// Shutdown stops transport work, waits for it, then closes all shared opens.
// Cleanup continues if the caller's context expires; later calls wait for it.
func (server *Server) Shutdown(ctx context.Context) error {
	server.mu.Lock()
	if !server.stopping {
		server.stopping = true
		var closeErr error
		for listener := range server.listeners {
			closeErr = errors.Join(closeErr, listener.close())
		}
		for connection := range server.connections {
			closeErr = errors.Join(closeErr, connection.close())
		}
		go server.drain(context.WithoutCancel(ctx), closeErr)
	}
	server.mu.Unlock()
	select {
	case <-server.shutdownDone:
		return server.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (server *Server) drain(ctx context.Context, closeErr error) {
	server.workers.Wait()
	server.shutdownErr = errors.Join(closeErr, server.cleanup(ctx, server.options.State.CloseAll()))
	close(server.shutdownDone)
}
