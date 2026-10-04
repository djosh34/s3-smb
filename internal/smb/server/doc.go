// Package server owns transport, sessions, trees, credits, compounds, async
// requests and SMB handlers. It joins wire, auth, crypt, state and smb.Storage.
// It must not reach into JuiceFS, reparse stream names or keep a second lock table.
//
// New validates the supplied modules and identity. The connection core handles
// negotiation and ECHO; session and file handlers are added in later milestones.
// The server never closes the storage runtime. Tests use ServeConn over net.Pipe
// without a listener or main wiring.
package server

import (
	"log/slog"
	"sync"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
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
	State      *state.Table
	Logger     *slog.Logger
	Now        func() time.Time
	Account    auth.Account
	ShareName  string
	ServerName string
	ServerGUID [16]byte
	Encryption EncryptionPolicy
}

// Server serves independent connections against one shared open table.
// Callers must use New.
// Serve owns the listener and accepts until cancellation or listener failure.
// It then drains requests, closes every open and waits for cleanup.
// ServeConn owns one connection and its ordered sender. Each queued frame has
// its own completion channel. A partial write error closes the connection and
// fails queued work without sending another frame.
// Shutdown stops accepting and drains requests. It closes every attached and
// detached open, applies pending deletion, and returns all cleanup errors.
// The app closes JuiceFS only after Shutdown returns. Repeated calls are safe.
type Server struct {
	shutdownErr  error
	handlers     map[wire.Command]handler
	connections  map[*connection]struct{}
	listeners    map[*ownedListener]struct{}
	shutdownDone chan struct{}
	options      Options
	workers      sync.WaitGroup
	mu           sync.Mutex
	stopping     bool
}
