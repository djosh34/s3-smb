package wire

import "github.com/djosh34/s3-smb/internal/smb"

// Negotiate context identifiers from MS-SMB2. Compression, transport, netname and
// unknown contexts are retained as raw contexts, but not advertised by the server.
const (
	ContextPreauth    uint16 = 0x0001
	ContextEncryption uint16 = 0x0002
	ContextSigning    uint16 = 0x0008
)

// PreauthContext contains hash offers or the one selected hash, plus salt.
type PreauthContext struct {
	Hashes []uint16
	Salt   []byte
}

// EncryptionContext contains cipher offers or the one selected GCM cipher.
type EncryptionContext struct {
	Ciphers []uint16
}

// SigningContext contains signing offers or the one selected algorithm.
type SigningContext struct {
	Algorithms []uint16
}

// AAPLQuery is the server-query command. Requested bits select server capabilities,
// volume capabilities and model information. Other AAPL commands are not supported.
type AAPLQuery struct {
	Requested          uint64
	ClientCapabilities uint64
}

// AAPLReply echoes supported requested bits and the exact feature policy. Model
// is UTF-16 encoded only when requested; padding and length are owned by wire.
type AAPLReply struct {
	Model              string
	Returned           uint64
	ServerCapabilities uint64
	VolumeCapabilities uint64
}

// MaxAccessQuery is MxAc's optional timestamp, preserved as FILETIME bits.
type MaxAccessQuery struct {
	Timestamp uint64
}

// MaxAccessReply reports MxAc's result, not the CREATE's overall status.
type MaxAccessReply struct {
	Status smb.Status
	Access uint32
}

// FileIDQuery requests QFid, without a request payload.
type FileIDQuery struct{}

// FileIDReply reports QFid's storage inode and stable volume identity, not an open ID.
type FileIDReply struct {
	DiskFileID uint64
	VolumeID   uint64
}

// DurableRequest is DH2Q. Timeout is milliseconds; flags retain a persistent
// request so the handler can refuse it, never silently echo unsupported flags.
type DurableRequest struct {
	CreateGUID [16]byte
	Timeout    uint32
	Flags      uint32
}

// DurableReply is a granted DH2Q. Its flags never grant persistence.
type DurableReply struct {
	Timeout uint32
	Flags   uint32
}

// DurableReconnect is DH2C. The whole wire ID and CreateGUID must match state.
type DurableReconnect struct {
	ID         FileID
	CreateGUID [16]byte
	Flags      uint32
}

// LeaseContext is RqLs, in either direction. Version is inferred from length and
// retained so a V1 request can receive no grant. V2 is the only advertised lease.
// ParentKey is parsed but directory leasing is not implemented. Duration remains
// a protocol field, not the server's break deadline or durable timeout.
type LeaseContext struct {
	Key       [16]byte
	ParentKey [16]byte
	Duration  uint64
	State     uint32
	Flags     uint32
	Epoch     uint16
	Version   uint16
}
