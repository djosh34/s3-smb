// Package smbtest owns the raw Go test client and real-adapter server fixtures.
// It must not emulate storage semantics or use fake filesystems to prove data
// coherence. It can send exact invalid bytes without weakening production codecs.
//
// M1 provides NewClient(conn net.Conn, codec wire.Codec) Client. It takes ownership
// of conn. M2 adds Start(ctx context.Context, options server.Options) (Fixture,
// error), listening on 127.0.0.1:0 with the supplied real adapter. Each test owns
// its JuiceFS runtime and must close the fixture before closing that runtime.
// No testing.T is hidden in constructors; every I/O and cleanup error is returned.
package smbtest

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// LoginOptions describes a normal 3.1.1 client handshake. Requested dialects,
// signing and cipher algorithms use smb constants. Tests that need other offers
// send raw wire messages rather than changing defaults or adding fallback code.
type LoginOptions struct {
	Address           string
	Share             string
	Account           auth.Account
	ClientGUID        [16]byte
	PreviousSessionID uint64
	Cipher            uint16
	Signing           uint16
	Encryption        bool
}

// Session is the identity and credit result of negotiate, setup and tree connect.
// Client handles crypt automatically after Login, including final setup verify.
type Session struct {
	SessionID uint64
	TreeID    uint32
	Credits   uint16
	Cipher    uint16
	Signing   uint16
}

// Reply carries all members from one received frame. Pending is derived from
// STATUS_PENDING and FlagAsync; the client must correlate BOTH MessageID and
// AsyncID, not treat the interim reply as the request's result.
type Reply struct {
	Messages []wire.Message
	Raw      []byte
}

// Client does not silently repair malformed headers, credits or compound flags.
// Send/Receive may run concurrently; one receiver owns incoming frames. There
// are no automatic retries. M5 reconnect uses a new Client and PreviousSessionID,
// then exact CREATE DH2C+RqLs messages with the retained file and lease identities.
type Client interface {
	// Login negotiates, authenticates, verifies setup and connects the named share.
	// It uses auth.Initiator and a crypt.RoleClient protector.
	Login(ctx context.Context, options LoginOptions) (Session, error)
	// Send encodes one complete compound payload, then protects and frames it.
	// Header IDs, flags and credit fields remain exactly as supplied.
	Send(ctx context.Context, messages []wire.Message) error
	// Receive reads a complete frame, verifies/decrypts and splits its members.
	// It returns interim and final replies separately, without discarding errors.
	Receive(ctx context.Context) (Reply, error)
	// SendRaw writes bytes verbatim, including any direct-TCP prefix, without
	// encoding, signing, encryption or credit accounting, for malformed tests.
	SendRaw(ctx context.Context, framed []byte) error
	// ReceiveRaw reads one direct-TCP frame and returns its unmodified payload.
	ReceiveRaw(ctx context.Context) ([]byte, error)
	// Close closes the connection and returns any transport error.
	Close() error
}

// Fixture owns the listener, server and connected test clients, not JuiceFS.
type Fixture interface {
	// Address returns the loopback host:port for raw clients and smbclient.
	Address() string
	// Close drains the server and returns shutdown errors.
	Close(ctx context.Context) error
}
