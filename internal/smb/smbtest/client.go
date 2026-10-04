// Package smbtest owns the raw Go test client and real-adapter server fixtures.
// It must not emulate storage semantics or use fake filesystems to prove data
// coherence. It can send exact invalid bytes without weakening production codecs.
//
// M1 provides NewClient(conn net.Conn) (*Client, error) after the wire PR (#200)
// merges. It rejects a nil connection and owns it on success. M1 has no login,
// signing or encryption in the client. M2 adds those behaviours and the fixture.
// Each test closes its fixture before closing the JuiceFS runtime it owns.
// Constructors do not hide testing.T; they return every I/O and cleanup error.
package smbtest

import (
	"net"
	"sync"

	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Reply carries the members from one received frame. The client identifies
// pending replies by STATUS_PENDING and FlagAsync. It correlates both MessageID
// and AsyncID and does not treat an interim reply as the request's result.
type Reply struct {
	Messages []wire.Message
	Raw      []byte
}

// Client preserves supplied headers, credits and compound flags. Send computes
// NextCommand links and padding; SendRaw bypasses all encoding. Receive checks
// pending and final async identities. ReceiveRaw bypasses that check, so callers
// must not mix it with Receive for a pending request.
// Send and Receive may run concurrently, with only one receiver. Context
// cancellation interrupts I/O. An I/O error closes the connection; requests are
// never retried. Callers must use NewClient and close the client when done.
// M2 adds protection after Login without repairing headers.
type Client struct {
	conn      net.Conn
	pending   map[uint64]uint64
	sendMu    sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// Fixture owns the listener, server and test clients, not JuiceFS. M2 provides
// Start(ctx context.Context, options server.Options) (*Fixture, error), listening
// on 127.0.0.1:0 with the supplied real adapter, and these methods.
// Address returns the loopback host:port. Close drains the server and returns
// shutdown errors. Callers must use Start.
//
//	func (fixture *Fixture) Address() string
//	func (fixture *Fixture) Close(ctx context.Context) error
type Fixture struct{}
