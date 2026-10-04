// Package state owns opens, sharing, deletion, ranges and leases in memory.
// It must not call storage, encode packets or depend on connections or JuiceFS.
// Time is injected. Methods are atomic and safe for concurrent callers; storage
// I/O and break delivery occur after they return, never under a table lock.
//
// M1 provides New(now func() time.Time) (*Table, error). It rejects a nil clock
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

// ShareMode uses the same bits as Rights. New rights must be allowed by every
// existing share mode, and existing rights must be allowed by the new share mode.
// Check and reservation occur before any create disposition can destroy bytes.
// Base-file delete access also checks deny-delete opens on every named stream.
type ShareMode Rights

// Open is a snapshot, not mutable table storage. ID.Persistent indexes it;
// Nonzero CreateGUID is also indexed by (client, user, share) for duplicate CREATE.
// Its handle, ranges, deletion intent and lease survive detachment. Explicit
// close, expiry and shutdown release them. CloseSession and CloseTree also close
// durable opens on logoff and tree disconnect. A transport drop does not.
// GrantedAccess retains the full expanded SMB mask through replay and reconnect.
// SharingIntent is only the read, write and delete intent used for share checks.
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
// M1 adds private indexes and implements the following method contracts.
// The zero value is not usable; callers must use New.
//
// Reserve atomically checks sharing in both directions and delete-pending.
//
//	func (table *Table) Reserve(request OpenRequest) (Reservation, smb.Status)
//
// Replay requires matching identity, GrantedAccess, SharingIntent and parameters.
// The server calls it only for a marked replay and does not repeat mutations.
//
//	func (table *Table) Replay(request OpenRequest) (Open, smb.Status)
//
// Commit converts a reservation into an open with a fresh FileID.
//
//	func (table *Table) Commit(reservation Reservation, grant Grant) (Open, smb.Status)
//
// Abort releases a failed CREATE reservation and its sharing intent.
//
//	func (table *Table) Abort(reservation Reservation) smb.Status
//
// Find validates both FileID halves and the binding.
//
//	func (table *Table) Find(id FileID, binding Binding) (Open, smb.Status)
//
// Close removes one attached open and returns its cleanup work.
//
//	func (table *Table) Close(id FileID, binding Binding) (CloseAction, smb.Status)
//
// SetDelete requires delete access and compatible sharing. Clearing one open's
// flag cannot clear another open's deletion intent.
//
//	func (table *Table) SetDelete(id FileID, binding Binding, name smb.Name, pending bool) smb.Status
//
// SetDirectory saves the cursor for one open.
//
//	func (table *Table) SetDirectory(id FileID, binding Binding, cursor DirectoryCursor) smb.Status
//
// Lock applies a whole vector atomically or changes nothing. It grants free
// ranges and returns STATUS_LOCK_NOT_GRANTED on conflict. This rule also applies
// when the request omits FAIL_IMMEDIATELY. No lock request waits.
//
//	func (table *Table) Lock(id FileID, binding Binding, ranges []Range, unlock bool) smb.Status
//
// CheckIO follows MS-FSA 2.1.4.10. Shared ranges block overlapping writes by
// every open, including their owner. Exclusive ranges block reads and writes by
// other opens but allow their owner's I/O. Shared ranges allow overlapping reads.
//
//	func (table *Table) CheckIO(id FileID, binding Binding, offset, length uint64, write bool) smb.Status
//
// Disconnect detaches durable opens and starts their granted timeout. It closes
// non-durable opens and returns their cleanup work.
//
//	func (table *Table) Disconnect(sessionID uint64) []CloseAction
//
// CloseSession closes every session open, including durable opens, on LOGOFF.
//
//	func (table *Table) CloseSession(sessionID uint64) []CloseAction
//
// CloseTree closes every open on the binding's tree, including durable opens.
//
//	func (table *Table) CloseTree(binding Binding) []CloseAction
//
// Reconnect changes the binding and volatile ID of a detached durable open.
// It preserves GrantedAccess and SharingIntent. Attached or expired opens fail.
//
//	func (table *Table) Reconnect(request ReconnectRequest) (Open, smb.Status)
//
// Expire closes detached opens whose deadline has passed, using the normal path.
//
//	func (table *Table) Expire() []CloseAction
//
// CloseAll returns cleanup for every open, including detached opens.
//
//	func (table *Table) CloseAll() []CloseAction
//
// BreakLeases starts breaks and captures all notification fields atomically.
// The server commits a conflicting CREATE only after the conflicting rights end.
//
//	func (table *Table) BreakLeases(object smb.ObjectKey, clientGUID GUID, leaseKey GUID, target uint32) []Break
//
// AckBreak validates the binding, client and lease key against the pending break.
// The acknowledged state must be a subset of its current target. There is no
// acknowledgment epoch on the wire; the reserved field is ignored by wire.
//
//	func (table *Table) AckBreak(binding Binding, clientGUID GUID, key GUID, leaseState uint32) smb.Status
//
// ExpireBreaks applies the target when a break times out. Losing H closes detached
// opens and removes durability from attached opens, which remain usable.
//
//	func (table *Table) ExpireBreaks() []CloseAction
type Table struct{}
