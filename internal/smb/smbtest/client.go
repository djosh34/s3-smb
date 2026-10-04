// Package smbtest owns the raw Go test client and the real JuiceFS storage
// that server tests run on. It must not emulate storage semantics or use fake
// filesystems to prove data coherence. It can send exact invalid bytes without
// weakening production codecs.
//
// NewClient rejects a nil connection and owns it on success. Login uses auth
// and crypt against a running server. Raw I/O bypasses encoding and protection.
// Each test closes its server before closing the JuiceFS runtime it owns.
// Client constructors return every I/O and cleanup error. NewStorage and
// NewS3Storage report setup and cleanup errors through testing.TB.
package smbtest

import (
	"net"
	"sync"

	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Reply carries the plaintext members from one received frame. Raw contains the
// exact framed payload, including the transform when encrypted. The client identifies
// pending replies by STATUS_PENDING and FlagAsync. It correlates both MessageID
// and AsyncID, checks SessionID, and returns interim replies separately.
type Reply struct {
	Messages []wire.Message
	Raw      []byte
}

// Client preserves supplied headers, credits and compound flags. Send computes
// NextCommand links and padding; SendRaw bypasses all encoding. Receive checks
// pending and final async identities. ReceiveRaw bypasses that check, so callers
// must not mix it with Receive for a pending request.
// Send and Receive may run concurrently, with only one receiver across Receive,
// WaitLeaseBreak and ReceiveRaw. Receive queues lease breaks instead of returning
// them as replies; WaitLeaseBreak queues normal replies instead. Do not mix
// ReceiveRaw with either decoded receiver. Context
// cancellation interrupts I/O. An I/O error closes the connection; requests are
// never retried. Callers must use NewClient and close the client when done.
// Login enables protection. Send changes signing flags and signatures, but
// preserves supplied identities and credits. Raw I/O remains unchanged.
type Client struct {
	conn              net.Conn
	protector         *crypt.Protector
	pending           map[uint64]pendingReply
	sendSlot          chan struct{}
	closeErr          error
	replies           []Reply
	leaseBreaks       []wire.LeaseBreakNotification
	protectionMu      sync.RWMutex
	sessionID         uint64
	closeOnce         sync.Once
	encrypted         bool // Outgoing requests and their replies use GCM.
	requireEncryption bool // SESSION_SETUP requires GCM for notifications too.
}

type pendingReply struct {
	asyncID   uint64
	sessionID uint64
}
