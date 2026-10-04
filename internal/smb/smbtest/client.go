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

import "github.com/djosh34/s3-smb/internal/smb/wire"

// Reply carries the members from one received frame. The client identifies
// pending replies by STATUS_PENDING and FlagAsync. It correlates both MessageID
// and AsyncID and does not treat an interim reply as the request's result.
type Reply struct {
	Messages []wire.Message
	Raw      []byte
}

// Client preserves supplied headers, credits and compound flags. M1 adds private
// transport state and the following methods. Callers must use NewClient.
// Send encodes and frames one complete compound payload.
// Receive reads and splits one complete frame. It returns each interim and final
// reply separately. M2 adds protection after Login, without repairing headers.
// SendRaw writes framed bytes verbatim without encoding or protection.
// ReceiveRaw reads one frame and returns its unmodified payload.
// Send and Receive may run concurrently, with only one receiver.
// Close returns any transport error. No method retries a request automatically.
//
//	func (client *Client) Send(ctx context.Context, messages []wire.Message) error
//	func (client *Client) Receive(ctx context.Context) (Reply, error)
//	func (client *Client) SendRaw(ctx context.Context, framed []byte) error
//	func (client *Client) ReceiveRaw(ctx context.Context) ([]byte, error)
//	func (client *Client) Close() error
type Client struct{}

// Fixture owns the listener, server and test clients, not JuiceFS. M2 provides
// Start(ctx context.Context, options server.Options) (*Fixture, error), listening
// on 127.0.0.1:0 with the supplied real adapter, and these methods.
// Address returns the loopback host:port. Close drains the server and returns
// shutdown errors. Callers must use Start.
//
//	func (fixture *Fixture) Address() string
//	func (fixture *Fixture) Close(ctx context.Context) error
type Fixture struct{}
