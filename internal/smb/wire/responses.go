package wire

// NegotiateResponse supplies the selected dialect and algorithms as contexts.
type NegotiateResponse struct {
	Token           []byte
	Contexts        []NegotiateContext
	ServerGUID      [16]byte
	SystemTime      uint64
	ServerStartTime uint64
	Capabilities    uint32
	MaxTransact     uint32
	MaxRead         uint32
	MaxWrite        uint32
	Dialect         uint16
	SecurityMode    uint16
}

// SessionSetupResponse carries the next SPNEGO token and session flags.
type SessionSetupResponse struct {
	Token []byte
	Flags uint16
}

// TreeConnectResponse describes the single disk share. No CA or DFS flags.
type TreeConnectResponse struct {
	Flags         uint32
	Capabilities  uint32
	MaximalAccess uint32
	ShareType     uint8
}

// CreateResponse returns an open ID, object attributes and only granted contexts.
type CreateResponse struct {
	Contexts       []CreateContext
	ID             FileID
	Created        uint64
	Accessed       uint64
	Modified       uint64
	Changed        uint64
	AllocationSize uint64
	Size           uint64
	Attributes     uint32
	Action         uint32
	Flags          uint8
	OplockLevel    uint8
}

// CloseResponse contains optional post-query attributes of the selected object.
type CloseResponse struct {
	Created        uint64
	Accessed       uint64
	Modified       uint64
	Changed        uint64
	AllocationSize uint64
	Size           uint64
	Attributes     uint32
	Flags          uint16
}

// ReadResponse owns returned data bytes.
type ReadResponse struct {
	Data      []byte
	Remaining uint32
}

// WriteResponse reports the actual committed-to-buffer byte count.
type WriteResponse struct {
	Count     uint32
	Remaining uint32
}

// QueryResponse carries class bytes or directory entries encoded by the typed
// functions in info_api.go. Unsupported classes are not decoded as another class.
type QueryResponse struct {
	Data []byte
}

// IOCTLResponse carries a validated control reply; unsupported codes use ErrorResponse.
type IOCTLResponse struct {
	Input       []byte
	Output      []byte
	ID          FileID
	ControlCode uint32
	Flags       uint32
}

// LeaseBreakNotification is the server's V2 break message, not an acknowledgement.
// CurrentState, NewState and AckRequired are captured by state when the break starts.
// Epoch is the notification's new lease epoch. Flags contains the ack-required bit.
type LeaseBreakNotification struct {
	Key            [16]byte
	CurrentState   uint32
	NewState       uint32
	Flags          uint32
	AccessMaskHint uint32
	ShareMaskHint  uint32
	BreakReason    uint32
	Epoch          uint16
}

// LeaseBreakResponse is an acknowledgement reply. Its reserved field is zero.
// It has no epoch and uses the lease state accepted by the table.
type LeaseBreakResponse struct {
	Key      [16]byte
	Duration uint64
	State    uint32
	Flags    uint32
}

// EmptyResponse represents ECHO, LOGOFF, TREE_DISCONNECT, FLUSH, LOCK and SET_INFO success.
// Their structure sizes are command-specific. STATUS_PENDING uses ErrorResponse.
type EmptyResponse struct{}
