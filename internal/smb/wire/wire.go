// Package wire owns pure, bounds-checked SMB codecs and command constants.
// It must not do I/O, authenticate, allocate credits or change server state.
// Decoders reject bad lengths, offsets, alignment, UTF-16 and context chains.
// Every decoder gets a native Go fuzz target. Returned bytes do not alias input.
//
// M1 implements the plain typed functions documented in api.go and info_api.go.
// No codec instance or type assertion is needed. The server distinguishes a
// well-framed unknown command from malformed framing and returns NOT_SUPPORTED.
// Raw test clients bypass body codecs by supplying Message.Body.
package wire

import "github.com/djosh34/s3-smb/internal/smb"

// Command is the SMB2 command number, not a dispatch array index.
type Command uint16

// Commands understood or explicitly refused by the server.
const (
	Negotiate      Command = 0x0000
	SessionSetup   Command = 0x0001
	Logoff         Command = 0x0002
	TreeConnect    Command = 0x0003
	TreeDisconnect Command = 0x0004
	Create         Command = 0x0005
	Close          Command = 0x0006
	Flush          Command = 0x0007
	Read           Command = 0x0008
	Write          Command = 0x0009
	Lock           Command = 0x000a
	IOCTL          Command = 0x000b
	Cancel         Command = 0x000c
	Echo           Command = 0x000d
	QueryDirectory Command = 0x000e
	ChangeNotify   Command = 0x000f
	QueryInfo      Command = 0x0010
	SetInfo        Command = 0x0011
	OplockBreak    Command = 0x0012
)

// HeaderFlags are bits in the SMB2 header.
type HeaderFlags uint32

const (
	// FlagResponse identifies a server response.
	FlagResponse HeaderFlags = 0x00000001
	// FlagAsync selects AsyncID instead of ProcessID and TreeID.
	FlagAsync HeaderFlags = 0x00000002
	// FlagRelated inherits session, tree and file identity from the preceding member.
	FlagRelated HeaderFlags = 0x00000004
	// FlagSigned carries a signature on unencrypted traffic.
	FlagSigned HeaderFlags = 0x00000008
	// FlagReplay marks a replay operation.
	FlagReplay HeaderFlags = 0x20000000
)

// Header is the 64-byte SMB2 header. NextCommand is a byte offset, checked before
// splitting. Credit is CreditRequest for requests and CreditResponse for replies.
// Async headers have no TreeID. FlagResponse selects Status; requests use
// ChannelSequence at that offset instead. Signing covers exact member bytes.
type Header struct {
	MessageID       uint64
	SessionID       uint64
	AsyncID         uint64
	Signature       [16]byte
	Status          smb.Status
	Flags           HeaderFlags
	NextCommand     uint32
	ProcessID       uint32
	TreeID          uint32
	Command         Command
	CreditCharge    uint16
	Credit          uint16
	ChannelSequence uint16
}

// FileID carries the persistent and volatile halves without depending on state.
type FileID struct {
	Persistent uint64
	Volatile   uint64
}

// Message is one compound member. Body excludes its header. Raw is the exact
// received header, body and padding used for verification, not a re-encoding.
type Message struct {
	Body   []byte
	Raw    []byte
	Header Header
}

// NegotiateContext contains one validated context's data, excluding its header.
// Type identifies preauth, encryption, signing, compression or netname. Unknown
// contexts are retained for the handler to ignore, never echoed by default.
type NegotiateContext struct {
	Data []byte
	Type uint16
}

// NegotiateRequest contains offered dialects and contexts in client preference order.
type NegotiateRequest struct {
	Dialects     []uint16
	Contexts     []NegotiateContext
	ClientGUID   [16]byte
	Capabilities uint32
	SecurityMode uint16
}

// SessionSetupRequest carries a SPNEGO token and previous session identity.
type SessionSetupRequest struct {
	Token             []byte
	PreviousSessionID uint64
	Capabilities      uint32
	SecurityMode      uint8
	Flags             uint8
}

// TreeConnectRequest contains a validated UTF-16 UNC path decoded to Go text.
type TreeConnectRequest struct {
	Path  string
	Flags uint16
}

// CreateContext carries a validated CREATE context. The codec checks chain
// lengths and overlap. Known tags: AAPL, MxAc, QFid, DH2Q, DH2C, RqLs. Context
// data decoders are supplied by M1 with round-trip and malformed-input tests.
type CreateContext struct {
	Name string
	Data []byte
}

// CreateRequest leaves path and stream interpretation to Storage.Lookup.
type CreateRequest struct {
	Name               string
	Contexts           []CreateContext
	DesiredAccess      uint32
	FileAttributes     uint32
	ShareAccess        uint32
	Disposition        uint32
	Options            uint32
	ImpersonationLevel uint32
	OplockLevel        uint8
}

// CloseRequest asks optionally for final attributes.
type CloseRequest struct {
	ID    FileID
	Flags uint16
}

// FlushRequest retains the Apple full-sync field unchanged.
type FlushRequest struct {
	ID        FileID
	Reserved1 uint16
}

// ReadRequest is bounded by MaxReadSize and the verified credit charge.
type ReadRequest struct {
	ChannelInfo    []byte
	ID             FileID
	Offset         uint64
	Length         uint32
	MinimumCount   uint32
	Channel        uint32
	RemainingBytes uint32
	Flags          uint8
}

// WriteRequest owns its data bytes. Flags retains WRITE_THROUGH.
type WriteRequest struct {
	Data           []byte
	ChannelInfo    []byte
	ID             FileID
	Offset         uint64
	Channel        uint32
	RemainingBytes uint32
	Flags          uint32
}

// LockElement is one validated range with raw SMB lock flags.
type LockElement struct {
	Offset uint64
	Length uint64
	Flags  uint32
}

// LockRequest carries the whole vector; state applies it atomically.
type LockRequest struct {
	Elements []LockElement
	ID       FileID
	Sequence uint32
}

// QueryDirectoryRequest keeps an empty continuation pattern distinct from "*".
type QueryDirectoryRequest struct {
	Pattern      string
	ID           FileID
	FileIndex    uint32
	OutputLength uint32
	InfoClass    DirectoryInfoClass
	Flags        uint8
}

// QueryInfoRequest carries file, filesystem or security information selection.
type QueryInfoRequest struct {
	Input                 []byte
	ID                    FileID
	OutputLength          uint32
	AdditionalInformation uint32
	Flags                 uint32
	InfoType              InfoType
	InfoClass             uint8
}

// SetInfoRequest owns its validated info-class bytes. info.go and info_api.go
// define their concrete types and codecs. Timestamp fields retain sentinel bits.
type SetInfoRequest struct {
	Input                 []byte
	ID                    FileID
	AdditionalInformation uint32
	InfoType              InfoType
	InfoClass             uint8
}

// IOCTLRequest represents a control code even when the handler will refuse it.
type IOCTLRequest struct {
	Input       []byte
	ID          FileID
	ControlCode uint32
	MaxOutput   uint32
	Flags       uint32
}

// LeaseBreakRequest carries an acknowledgement, never a CREATE lease grant.
// Its two-byte reserved field is ignored on decode and written as zero.
// An acknowledgement has no epoch; Epoch belongs to the notification.
type LeaseBreakRequest struct {
	Key      [16]byte
	Duration uint64
	State    uint32
	Flags    uint32
}

// ChangeNotifyRequest is decoded even though the handler returns NOT_SUPPORTED.
type ChangeNotifyRequest struct {
	ID           FileID
	OutputLength uint32
	Filter       uint32
	Flags        uint16
}

// EmptyRequest is used for ECHO, LOGOFF, TREE_DISCONNECT and CANCEL.
type EmptyRequest struct{}

// ErrorResponse carries error contexts. Ordinary errors use an empty Data slice.
type ErrorResponse struct {
	Data         []byte
	ContextCount uint8
}
