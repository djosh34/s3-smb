// Package server owns transport, sessions, trees, credits, compounds, async
// requests and SMB handlers. It joins wire, auth, crypt, state and smb.Storage.
// It must not reach into storage internals or parse storage paths.
//
// New validates the supplied modules and identity. The server handles negotiation,
// NTLMv2 sessions, disk-share trees, ECHO, signing and GCM encryption. File
// handlers are registered in handlers.go, one line per command. They resolve
// request IDs with RequestContext.FileID and report the ID used or created in
// reply.fileID. Compounds save that ID for the next related member. Handlers run
// open-table CloseActions through RequestContext.Cleanup. A related command that
// needs a FileId inherits an error-severity predecessor status without running;
// warning statuses allow it to run.
// The server never closes the storage runtime.
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

// Options joins the storage, open table, logger and clock. ShareName is the only disk share.
// State must be constructed with Now so authentication and expiry share a clock.
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
// The first Serve or ServeConn starts one expiry timer shared by all connections.
// Shutdown stops the timer, stops accepting and drains requests and expiry
// cleanup. It closes every attached and detached open, applies pending deletion,
// and returns cleanup errors.
// The app shuts storage down only after Shutdown returns. Repeated calls are safe.
type Server struct {
	shutdownErr          error
	connectionCleanupErr error
	activeOpens          map[uint64]*openUses
	parents              map[smb.Inode]*parentGuard
	handlers             map[wire.Command]handler
	connections          map[*connection]struct{}
	clients              map[*connection][16]byte
	sessions             map[uint64]*connection
	listeners            map[*ownedListener]struct{}
	shutdownDone         chan struct{}
	scavengerStop        chan struct{}
	scavengerDone        chan struct{}
	options              Options
	workers              sync.WaitGroup
	scavengerCleanup     sync.WaitGroup
	mu                   sync.Mutex
	openMu               sync.Mutex
	namespaceMu          sync.Mutex
	nextSessionID        uint64
	nextTreeID           uint32
	stopping             bool
}
