// Package state owns opens, sharing, deletion, ranges and leases in memory.
// It must not call storage, encode packets or depend on connections or JuiceFS.
// Time is injected. Methods are atomic and safe for concurrent callers; storage
// I/O and break delivery occur after they return, never under a table lock.
//
// New rejects a nil clock and allocates empty indexes. Callers deliver returned
// lease breaks and cleanup actions after table methods return.
package state

import (
	"sync"
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

// Rights is the normalized read, write and delete sharing intent. Metadata-only
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

// ShareMode uses the same bits as Rights. When both same-stream opens have
// sharing intent, new rights must be allowed by the existing share mode and
// existing rights by the new share mode. Metadata-only opens do not participate,
// including in base-file delete checks against named streams.
// Check and reservation occur before any create disposition can destroy bytes.
// Base-file delete access also checks every named stream with sharing intent.
type ShareMode Rights

// Open is a snapshot, not mutable table storage. ID.Persistent indexes it;
// Nonzero CreateGUID is also indexed by (client, user, share) for duplicate CREATE.
// Its handle, ranges, deletion intent and lease survive detachment. Explicit
// close, expiry and shutdown release them. CloseSession and CloseTree also close
// durable opens on logoff and tree disconnect. A transport drop does not.
// GrantedAccess retains the full expanded SMB mask through replay and reconnect.
// SharingIntent includes the minimum read, write and delete intent from the
// granted mask. DeleteOnClose records the CREATE option; SET_INFO disposition
// is tracked separately and makes the object delete-pending at once.
// This table is never persisted across restart.
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
	GrantedAccess    uint32
	SharingIntent    Rights
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
// A nonempty range's last byte is Offset+Length-1, checked before reservation.
// The last byte may be 2^64-1; a larger value returns INVALID_LOCK_RANGE.
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
// cannot exceed BreakTo. Timeout revokes the whole lease. H is required for a
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
// DeletePending remains set until CompleteDelete reports the cleanup outcome.
// A base deletion waits for all opens on that inode, including named streams;
// a stream deletion waits only for that stream and never removes the base.
// Records are removed only after opens, reservations, locks and leases are gone.
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
// existing file Reserve precedes truncate, supersede or any other mutation.
// GrantedAccess includes append and metadata rights, not just SharingIntent.
// Reserve adds the mask's minimum sharing intent; callers may add delete for
// supersede even when the mask does not contain DELETE.
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
	GrantedAccess    uint32
	SharingIntent    Rights
	Sharing          ShareMode
}

// Reservation is an opaque token. It participates in share checks until Commit
// or Abort, exactly once. It prevents another open from slipping between the
// share check and adapter Open or Truncate. No table mutex is held by the caller.
type Reservation uint64

// Grant supplies storage and CREATE results for Commit. A durable grant requires
// an H lease on a regular unnamed file and a timeout in (0, MaxDurableTimeout].
// Zero timeout means no durable grant. The handler normalizes a client's requested
// timeout to the default or maximum before Commit; this is a granted timeout.
type Grant struct {
	Handle         smb.Handle
	DeleteName     smb.Name
	Lease          Lease
	DurableTimeout time.Duration
	Directory      bool
	DeleteOnClose  bool
}

// CloseAction transfers cleanup to the server. FileID names the removed open;
// active references use its persistent half across reconnects. The table has
// already removed the open and ranges. The server closes Handle and, if Remove
// is true, calls identity-checked Remove after resolving the current name under
// a parent guard.
// Object and Name identify the deletion, which may be a pending base deletion
// triggered by the last stream close, not Handle.Key().
// Cleanup failures propagate, but cannot restore a half-closed open. The server
// retains delete-pending until CompleteDelete on every removal outcome, and
// drains active request references before closing Handle without a parent guard.
// A transport drop cannot close a storage reference still in use by an async
// request.
type CloseAction struct {
	Handle smb.Handle
	Object smb.ObjectKey
	Name   smb.Name
	FileID FileID
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
// The table captures CurrentState and AckRequired when it starts the break.
// The server waits asynchronously before committing a conflicting CREATE.
type Break struct {
	Binding      Binding
	ClientGUID   GUID
	LeaseKey     GUID
	CurrentState uint32
	NewState     uint32
	Epoch        uint16
	AckRequired  bool
}

// Table owns all indexes. Returned structs and slices are copies. Failed methods
// leave state unchanged. Status-returning methods return StatusSuccess on success,
// otherwise a command-specific status, such as SHARING_VIOLATION, DELETE_PENDING,
// LOCK_NOT_GRANTED, FILE_LOCK_CONFLICT, RANGE_NOT_LOCKED or DUPLICATE_OBJECTID.
// Detached durable opens still participate in every sharing and lock check.
// The zero value is not usable; callers must use New.
type Table struct {
	now             func() time.Time
	opens           map[uint64]*openEntry
	reservations    map[Reservation]OpenRequest
	objects         map[smb.ObjectKey]*objectEntry
	creates         map[createIdentity]createEntry
	leaseObjects    map[leaseIdentity]smb.ObjectKey
	mu              sync.Mutex
	nextReservation uint64
	nextPersistent  uint64
	nextVolatile    uint64
}

type openEntry struct {
	deleteName smb.Name
	Open
	dispositionPending bool
}

type objectEntry struct {
	ObjectRecord
	deleteCommitted bool
	removalPending  bool
}

type createIdentity struct {
	user   string
	share  string
	client GUID
	create GUID
}

type createEntry struct {
	reservation Reservation
	persistent  uint64
}

type leaseIdentity struct {
	client GUID
	key    GUID
}
