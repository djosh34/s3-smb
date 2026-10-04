// Package state owns opens, sharing, deletion, ranges and leases in memory.
// It must not call storage, encode packets or depend on connections or JuiceFS.
// Time is injected. Methods are atomic and safe for concurrent callers; storage
// I/O and break delivery occur after they return, never under a table lock.
//
// M1 provides New(now func() time.Time) (Table, error). It rejects a nil clock
// and allocates empty indexes. M1 implements every pure table transition; M5
// connects lease breaks and durable transitions to the server's protocol handlers.
package state

import (
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// FileID is the wire identity. Persistent is the open-table key, not an inode.
// Volatile is regenerated on reconnect, so old connections cannot use new opens.
// Neither half is zero or all-ones; all-ones is a related-compound placeholder.
type FileID struct {
	Persistent uint64
	Volatile   uint64
}

// GUID identifies a CREATE, client or lease. The bytes are in wire order.
type GUID [16]byte

// Binding identifies an attached open. Zero SessionID means detached; TreeID
// must then also be zero. There is no connection pointer or connection key.
type Binding struct {
	SessionID uint64
	TreeID    uint32
}

// Rights is the normalized read/write/delete sharing intent. Metadata-only
// access does not imply data read. Generic rights are expanded by the server.
type Rights uint8

const (
	// RightRead includes data read or execute.
	RightRead Rights = 1 << iota
	// RightWrite includes data write or append.
	RightWrite
	// RightDelete includes deletion and supersede.
	RightDelete
)

// ShareMode uses the same bits as Rights. New rights must be allowed by every
// existing share mode AND existing rights must be allowed by the new share mode.
// Check and reservation occur before any create disposition can destroy bytes.
// Base-file delete access also checks deny-delete opens on every named stream.
type ShareMode Rights

// Open is a snapshot, not mutable table storage. ID.Persistent indexes it;
// Nonzero CreateGUID is also indexed by (client, user, share) for duplicate CREATE.
// Its handle, ranges, deletion intent and lease survive detachment. Only an
// explicitly closed, expired or shutdown open releases them. Logoff closes its
// opens, unlike a transport drop. This table is never persisted across restart.
type Open struct {
	DurableDeadline  time.Time
	Handle           smb.Handle
	User             string
	Share            string
	Directory        DirectoryCursor
	Object           smb.ObjectKey
	ID               FileID
	Binding          Binding
	ClientGUID       GUID
	CreateGUID       GUID
	CreateParameters [32]byte
	LeaseKey         GUID
	DurableTimeout   time.Duration
	Access           Rights
	Sharing          ShareMode
	DeleteOnClose    bool
	Durable          bool
}

// DirectoryCursor belongs to one SMB open. Empty continuation patterns reuse
// Pattern. Restart clears Cookie; reopening starts a new search.
type DirectoryCursor struct {
	Pattern string
	Cookie  smb.Cookie
}

// Range describes a non-blocking byte lock owned by one persistent FileId.
// End is Offset+Length (exclusive), checked for overflow before reservation.
// Zero-length ranges follow MS-SMB2 zero-byte rules, not arithmetic overlap.
// Unlock requires the exact owner, offset and length, not just overlap. Closing
// an open releases every range it owns, including when durability expires.
type Range struct {
	Owner     uint64
	Offset    uint64
	Length    uint64
	Exclusive bool
}

// Lease tracks a V2 lease shared by opens of the same client and key on one
// object. A break only loses rights. Epoch advances per V2 rules; pending grants
// cannot exceed BreakTo until acknowledged or timed out. H is required for a
// durable grant. Directory and named-stream opens receive no lease or durability.
// A client's lease key identifies only one object; reuse on another is rejected.
type Lease struct {
	Deadline   time.Time
	ClientGUID GUID
	Key        GUID
	State      uint32
	BreakTo    uint32
	Epoch      uint16
	Breaking   bool
}

// ObjectRecord describes the per-(inode, stream) record. Opens, locks and leases
// never cross stream keys. DeletePending rejects new opens. DeleteName is the
// name selected for deletion, not the name of the last closing handle. For a
// renamed base, the close path resolves PathOf and verifies the inode again.
// A base deletion waits for all opens on that inode, including named streams;
// a stream deletion waits only for that stream and never removes the base.
// Records are removed only after opens/reservations, locks and leases are gone.
type ObjectRecord struct {
	Opens         []uint64
	Locks         []Range
	Leases        []Lease
	Key           smb.ObjectKey
	DeleteName    smb.Name
	DeletePending bool
}

// OpenRequest contains everything needed for an atomic sharing reservation.
// Object.Inode must be nonzero. For a new file the server holds its parent guard,
// creates an identity, then reserves it before releasing that guard. For an
// existing file Reserve precedes truncate/supersede or any other mutation.
// CreateParameters is the server's SHA-256 of canonical CREATE parameters,
// including name, disposition, options and requested contexts, for replay checks.
type OpenRequest struct {
	User             string
	Share            string
	Object           smb.ObjectKey
	Binding          Binding
	ClientGUID       GUID
	CreateGUID       GUID
	CreateParameters [32]byte
	Access           Rights
	Sharing          ShareMode
}

// Reservation is an opaque token. It participates in share checks until Commit
// or Abort, exactly once. It prevents another open from slipping between the
// share check and adapter Open/Truncate. No table mutex is held by the caller.
type Reservation uint64

// Grant supplies storage and CREATE results for Commit. A durable grant requires
// an H lease on a regular unnamed file and a timeout in (0, MaxDurableTimeout].
type Grant struct {
	Handle         smb.Handle
	DeleteName     smb.Name
	Lease          Lease
	DurableTimeout time.Duration
	Directory      bool
	DeleteOnClose  bool
}

// CloseAction transfers cleanup to the server. The table has already removed
// the open and ranges. The server closes Handle and, if Remove is true, calls
// identity-checked Remove after resolving the current name under a parent guard.
// Object and Name identify the deletion, which may be a pending base deletion
// triggered by the last stream close, not Handle.Key().
// Cleanup failures propagate, but cannot restore a half-closed open. The server
// blocks new opens through the guard until deletion finishes, and drains active
// request references before closing Handle. A transport drop cannot close a
// storage reference still in use by an async request.
type CloseAction struct {
	Handle smb.Handle
	Object smb.ObjectKey
	Name   smb.Name
	Remove bool
}

// ReconnectRequest must match every identity, including user, share, client,
// CreateGUID and lease key. The detached deadline must be strictly in the future.
// Reconnect changes only binding and volatile ID, never rights, handles or locks.
type ReconnectRequest struct {
	User       string
	Share      string
	ID         FileID
	Binding    Binding
	ClientGUID GUID
	CreateGUID GUID
	LeaseKey   GUID
}

// Break is work for the server's sender after the state lock is released.
// The server waits asynchronously before committing a conflicting CREATE.
type Break struct {
	Binding    Binding
	ClientGUID GUID
	LeaseKey   GUID
	NewState   uint32
	Epoch      uint16
}

// Table owns all indexes. Returned structs and slices are copies. Failed methods
// leave state unchanged. Status-returning methods return StatusSuccess on success,
// otherwise a command-specific status, such as SHARING_VIOLATION, DELETE_PENDING,
// LOCK_NOT_GRANTED, FILE_LOCK_CONFLICT, RANGE_NOT_LOCKED or DUPLICATE_OBJECTID.
// Detached durable opens still participate in every sharing and lock check.
type Table interface {
	// Reserve atomically checks both directions of sharing and delete-pending.
	Reserve(request OpenRequest) (Reservation, smb.Status)
	// Replay returns an already committed CREATE only when all request identity
	// fields and CreateParameters match. The server calls it only with REPLAY
	// set, reuses the same open ID and grant, and does not repeat storage mutation.
	Replay(request OpenRequest) (Open, smb.Status)
	// Commit converts a reservation into an open with a fresh FileID.
	Commit(reservation Reservation, grant Grant) (Open, smb.Status)
	// Abort releases a failed CREATE reservation, including its share rights.
	Abort(reservation Reservation) smb.Status
	// Find validates both ID halves and binding. Inode lookup is never sufficient.
	Find(id FileID, binding Binding) (Open, smb.Status)
	// Close removes an attached open and returns any required cleanup.
	Close(id FileID, binding Binding) (CloseAction, smb.Status)
	// SetDelete requires delete rights and compatible sharing. Clearing the flag
	// cannot clear another open's deletion intent. Name is identity-checked later.
	SetDelete(id FileID, binding Binding, name smb.Name, pending bool) smb.Status
	// SetDirectory saves the cursor for this open, not in the adapter.
	SetDirectory(id FileID, binding Binding, cursor DirectoryCursor) smb.Status
	// Lock applies a whole LOCK vector atomically, or changes nothing. wait=true
	// fails immediately with NOT_SUPPORTED; there is no blocking queue.
	Lock(id FileID, binding Binding, ranges []Range, unlock bool, wait bool) smb.Status
	// CheckIO checks ranges on this exact object. Own ranges do not conflict;
	// other exclusive ranges block reads and other ranges block writes.
	CheckIO(id FileID, binding Binding, offset, length uint64, write bool) smb.Status
	// Disconnect detaches durable opens and sets now+granted timeout. It closes
	// non-durable opens and returns their cleanup work. Transport IDs are absent.
	Disconnect(sessionID uint64) []CloseAction
	// CloseSession closes even durable opens on explicit LOGOFF.
	CloseSession(sessionID uint64) []CloseAction
	// Reconnect atomically reattaches one detached durable open, with a new
	// volatile ID. An attached, expired or mismatched open cannot be stolen.
	Reconnect(request ReconnectRequest) (Open, smb.Status)
	// Expire closes detached opens at deadline <= now through the normal path.
	Expire() []CloseAction
	// CloseAll returns cleanup for all opens, including detached ones, at shutdown.
	CloseAll() []CloseAction
	// BreakLeases starts required breaks and returns sender work. No conflicting
	// CREATE is committed until the affected leases lose the conflicting rights.
	BreakLeases(object smb.ObjectKey, clientGUID GUID, leaseKey GUID, target uint32) []Break
	// AckBreak verifies binding, key and epoch before reducing lease state.
	AckBreak(binding Binding, key GUID, epoch uint16, leaseState uint32) smb.Status
	// ExpireBreaks applies the target on timeout and closes detached opens whose
	// H protection was lost. Attached opens remain usable but lose durability
	// when H is lost. Uses the injected clock.
	ExpireBreaks() []CloseAction
}
