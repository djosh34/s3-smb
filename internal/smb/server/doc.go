// Package server owns transport, sessions, trees, credits, compounds, async
// requests and SMB handlers. It joins wire, auth, crypt, state and smb.Storage.
// It must not reach into JuiceFS, reparse stream names or keep a second lock table.
//
// M2 provides New(options Options) (Server, error). It validates all inputs,
// constructs wire/auth/crypt instances through their M1 constructors and uses the
// supplied state table and storage. The server never closes the storage runtime.
// Tests use ServeConn over net.Pipe without a listener or main wiring.
package server

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

// EncryptionPolicy selects session confidentiality, not the server implementation.
// The zero value requires GCM; AllowPlaintext still requires signing.
type EncryptionPolicy uint8

const (
	// RequireEncryption refuses clients that offer no GCM cipher.
	RequireEncryption EncryptionPolicy = iota
	// AllowPlaintext permits signed plaintext sessions and negotiated GCM sessions.
	AllowPlaintext
)

// Options joins the independent M1 modules. ShareName is the only disk share.
// ServerGUID is stable for the running daemon. Now drives deadlines; Logger logs
// rejected frames and negotiation reasons without passwords, tokens or keys.
type Options struct {
	Storage    smb.Storage
	State      state.Table
	Logger     *slog.Logger
	Now        func() time.Time
	Account    auth.Account
	ShareName  string
	ServerName string
	ServerGUID [16]byte
	Encryption EncryptionPolicy
}

// Server serves independent connections against one shared open table.
type Server interface {
	// Serve accepts until cancellation or listener failure. It owns and closes
	// the listener, drains requests, closes all opens and waits for cleanup.
	Serve(ctx context.Context, listener net.Listener) error
	// ServeConn owns one connection, including its ordered sender. Each queued
	// frame has its own completion channel. Any partial write error closes the
	// connection and fails all queued work, without sending another frame.
	ServeConn(ctx context.Context, conn net.Conn) error
	// Shutdown stops accepting, drains requests, closes attached and detached
	// opens with delete-on-close, and returns all cleanup errors. The app may
	// close JuiceFS only after this returns. It is safe to call more than once.
	Shutdown(ctx context.Context) error
}
