// Package smb defines the storage seam, status codes and feature policy for the
// new server. It must not decode packets, keep opens or call JuiceFS.
package smb

import (
	"context"
	"time"
)

// Inode is a stable storage identity. Zero is invalid, not the share root.
type Inode uint64

// ObjectKey identifies one data object. Stream is the adapter's canonical xattr
// name; empty means the unnamed data stream. All SMB state uses this whole key.
// A base file and its named streams share an inode, but not locks or deletion.
type ObjectKey struct {
	Stream string
	Inode  Inode
}

// Name identifies a directory entry and its selected stream. Parent is never
// zero. Base contains no separators. Stream uses the same spelling as ObjectKey.
// Only the adapter parses SMB paths and stream syntax into this form.
type Name struct {
	Base   string
	Stream string
	Parent Inode
}

// Resolved is Lookup's result, including a missing final object. If Exists is
// false, Name remains usable by Create. Object.Inode is zero only when the base
// entry is missing; a missing stream retains the existing base inode.
type Resolved struct {
	Name   Name
	Object ObjectKey
	Attr   Attr
	Exists bool
}

// Access is storage access, not an SMB access mask. The server expands generic
// rights and checks SMB permissions before choosing these bits.
type Access uint8

const (
	// AccessRead permits reading data.
	AccessRead Access = 1 << iota
	// AccessWrite permits writing and truncating data.
	AccessWrite
)

// Handle is an adapter-owned file reference and access mode, not an SMB open.
// The adapter may keep per-inode coherence state, but no open registry, share
// modes, delete-pending, leases or locks. No JuiceFS plocks are used.
// Key is immutable and selects file or stream once; handlers must not branch on it.
type Handle interface {
	Key() ObjectKey
}

// Kind describes the storage object. Streams are regular data objects.
type Kind uint8

const (
	// KindFile is a regular file or named stream.
	KindFile Kind = iota
	// KindDirectory is a directory and cannot have stream data opened as a directory.
	KindDirectory
)

// Attr describes the selected object's current state, including buffered writes.
// Times are UTC; Size and AllocationSize are bytes. Inode is never a handle ID.
type Attr struct {
	Created        time.Time
	Accessed       time.Time
	Modified       time.Time
	Changed        time.Time
	Inode          Inode
	Size           uint64
	AllocationSize uint64
	Attributes     uint32
	Kind           Kind
}

// AttrChange uses pointers to distinguish absent values from zero or Unix epoch.
// The wire layer handles FILETIME sentinels before forming this value.
type AttrChange struct {
	Created    *time.Time
	Accessed   *time.Time
	Modified   *time.Time
	Changed    *time.Time
	Size       *uint64
	Attributes *uint32
}

// SyncMode selects the local metadata barrier; both modes wait for S3 data.
type SyncMode uint8

const (
	// SyncData commits data to S3 and metadata durably on local disk.
	SyncData SyncMode = iota
	// SyncFull also uses a full-fsync barrier for the committed local metadata.
	SyncFull
)

// Cookie is an opaque directory position. Zero starts an enumeration. The SMB
// open owns the cookie and pattern; the adapter keeps no directory cursor.
type Cookie uint64

// DirEntry is a directory entry with live per-inode attributes and a next cookie.
type DirEntry struct {
	Name string
	Attr Attr
	Next Cookie
}

// StreamInfo describes a canonical named stream, without the :$DATA suffix.
type StreamInfo struct {
	Name           string
	Size           uint64
	AllocationSize uint64
}

// Space describes bytes, not SMB allocation units. Capacity is configured
// storage.capacity when set. Without it, Free is capped at 1 TiB. Available is
// the caller's usable free space, never greater than Free.
type Space struct {
	VolumeID  uint64
	Capacity  uint64
	Free      uint64
	Available uint64
}

// RenameRequest identifies both names, including expected source and destination
// identities. DestinationInode zero requires an absent destination. Replace may
// replace only DestinationInode; a changed identity fails without any mutation.
// Source and Destination must name base entries. A nonempty Stream on either
// name returns ErrNotSupported without changing data or namespace identity.
type RenameRequest struct {
	Source           Name
	Destination      Name
	SourceInode      Inode
	DestinationInode Inode
	Replace          bool
}

// Storage is the complete filesystem interface. Methods accept cancellation and
// return wrapped ErrorKind values, preserving the backend cause for logs. There
// is no handle zero, root handle convention or alternate stream I/O interface.
//
// Coherence is per inode across every handle. Reads see acknowledged writes;
// Flush, Truncate and SetAttr coordinate with the shared writer. Lookup, GetAttr
// and ReadDir include buffered size without uploading data. A later flush cannot
// undo an acknowledged truncate or explicit timestamp change (#84, #89, #96,
// #113). No storage or network I/O runs under a global share lock (#59).
// Namespace mutations may serialize by parent; unrelated inodes must progress.
//
// The server owns SMB opens and guards namespace lookup, checks and mutations
// by parent before reserving share access. Storage never enforces SMB sharing.
// Methods must not call back into state or the server while holding inode locks.
type Storage interface {
	// Lookup resolves a share-relative SMB name once, including stream syntax.
	// Empty path names the root. It rejects traversal, trash and special nodes.
	// A missing final component returns Exists=false, not ErrNameNotFound;
	// missing ancestors return ErrPathNotFound. Attr is valid only when Exists.
	Lookup(ctx context.Context, path string) (Resolved, error)
	// Open takes an existing identity, never creates or truncates it. It returns
	// a handle whose Key is exactly object. Zero inode is an error.
	Open(ctx context.Context, object ObjectKey, access Access) (Handle, error)
	// Create exclusively creates the selected object. A stream requires its
	// base file to exist. Existing objects return ErrNameCollision. It returns
	// the identity but does not open it. Supersede is a server-controlled mutation.
	Create(ctx context.Context, name Name, kind Kind) (Resolved, error)
	// Close releases one storage reference exactly once. Flush errors propagate.
	Close(ctx context.Context, handle Handle) error
	// ReadAt reads the selected object. A short read returns its byte count and
	// io.EOF; the server sends bytes if n>0, or STATUS_END_OF_FILE if n==0.
	ReadAt(ctx context.Context, handle Handle, dst []byte, offset uint64) (int, error)
	// WriteAt writes the selected object. Short writes always return an error.
	// Streams obey the same offset and hole rules, with a 64 KiB size limit.
	WriteAt(ctx context.Context, handle Handle, src []byte, offset uint64) (int, error)
	// Flush covers all writes completed before the call on this inode, even
	// from other handles. Both modes return only after data is in S3 and local
	// metadata is durable. SyncFull also commits with a full-fsync barrier.
	// A named stream flush commits its xattr metadata with the same barrier.
	// The server maps FLUSH Reserved1=0xffff to SyncFull and 0 to SyncData.
	Flush(ctx context.Context, handle Handle, mode SyncMode) error
	// Truncate changes the selected object's length coherently. Pending writes
	// cannot later resurrect removed bytes. Extensions read as zeroes.
	Truncate(ctx context.Context, handle Handle, size uint64) error
	// GetAttr returns the selected object's live size, including buffered data,
	// without a flush. Streams never report the base file's length.
	GetAttr(ctx context.Context, object ObjectKey) (Attr, error)
	// SetAttr applies changes in order with buffered writes on the inode. Size
	// changes use the Truncate contract. Explicit times survive subsequent flush.
	SetAttr(ctx context.Context, object ObjectKey, change AttrChange) error
	// ReadDir returns at most limit entries with live attributes, starting at
	// cookie. At exhaustion it returns an empty slice and nil error. Cookies
	// remain usable without adapter cursor state; the server owns filtering.
	ReadDir(ctx context.Context, inode Inode, cookie Cookie, limit uint32) ([]DirEntry, error)
	// Streams lists named streams for a base inode; unnamed data is not listed.
	Streams(ctx context.Context, inode Inode) ([]StreamInfo, error)
	// Remove deletes exactly name if its base inode is expect. For a stream it
	// removes only that xattr. Identity mismatch leaves the namespace unchanged.
	Remove(ctx context.Context, name Name, expect Inode) error
	// Rename moves exactly the expected identities, atomically. It does not
	// rewrite handle paths; identities and open-table keys stay unchanged.
	// Named-stream rename returns ErrNotSupported, mapped to STATUS_NOT_SUPPORTED.
	Rename(ctx context.Context, request RenameRequest) error
	// PathOf returns the current share-relative base path. No hard links are
	// supported, so a linked inode has one path. An unlinked inode is not found.
	PathOf(ctx context.Context, inode Inode) (string, error)
	// StatFS reports volume identity and configured capacity, independent of handles.
	StatFS(ctx context.Context) (Space, error)
}
