// Package smb defines the storage seam, status codes and feature policy for the
// server. It must not decode packets or keep opens.
package smb

import (
	"context"
	"time"
)

// Inode is a stable storage identity. Zero is invalid, not the share root.
type Inode uint64

// Name identifies a directory entry. Parent is never zero. Base contains no
// separators. Only storage parses SMB paths into this form.
type Name struct {
	Base   string
	Parent Inode
}

// Resolved is Lookup's result, including a missing final object. If Exists is
// false, Name remains usable by Create and Object is zero.
type Resolved struct {
	Name   Name
	Attr   Attr
	Object Inode
	Exists bool
}

// Access is storage access, not an SMB access mask. The server expands generic
// rights and checks SMB permissions before choosing these bits.
type Access uint8

const (
	// AccessRead permits reading data.
	AccessRead Access = 1 << iota
	// AccessWrite permits writing and truncating data. AccessAppend, when also
	// set, restricts writes but leaves truncate permission intact.
	AccessWrite
	// AccessAppend permits writes only at or beyond the file's live EOF, even
	// with AccessWrite. Alone it does not permit truncation.
	AccessAppend
)

// Handle is a storage-owned file reference and access mode, not an SMB open.
// Storage may keep per-inode coherence state, but no open registry, share
// modes, delete-pending or leases. Key is immutable.
type Handle interface {
	Key() Inode
}

// Kind describes the storage object.
type Kind uint8

const (
	// KindFile is a regular file.
	KindFile Kind = iota
	// KindDirectory is a directory.
	KindDirectory
)

// Attr describes the object's current state, including buffered writes.
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
// Size sets EOF; SizeCap only shrinks it, comparing against live EOF under
// storage's per-inode lock. They are mutually exclusive. A SizeCap
// at or above EOF changes neither length nor automatic timestamps.
type AttrChange struct {
	Created    *time.Time
	Accessed   *time.Time
	Modified   *time.Time
	Changed    *time.Time
	Size       *uint64
	SizeCap    *uint64
	Attributes *uint32
}

// SyncMode selects what a flush covers; both modes wait for S3 data.
type SyncMode uint8

const (
	// SyncData commits one file's data to S3 and its metadata durably on
	// local disk.
	SyncData SyncMode = iota
	// SyncFull does the same for every file.
	SyncFull
)

// Cookie is an opaque directory position. Zero starts an enumeration. The SMB
// open owns the cookie and pattern; storage keeps no directory cursor.
type Cookie uint64

// DirEntry is a directory entry with live per-inode attributes and a next cookie.
type DirEntry struct {
	Name string
	Attr Attr
	Next Cookie
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
type RenameRequest struct {
	Source           Name
	Destination      Name
	SourceInode      Inode
	DestinationInode Inode
	Replace          bool
}

// Storage is the complete filesystem interface. Methods accept cancellation and
// return wrapped ErrorKind values, preserving the backend cause for logs. There
// is no handle zero or root handle convention.
//
// Coherence is per inode across every handle. Reads see acknowledged writes;
// Flush, Truncate and SetAttr coordinate with the shared writer. Lookup, GetAttr
// and ReadDir include buffered size without uploading data. A later flush cannot
// undo an acknowledged truncate or explicit timestamp change. No storage or
// network I/O runs under a global share lock.
// Namespace mutations may serialize by parent; unrelated inodes must progress.
//
// The server owns SMB opens and guards namespace lookup, checks and mutations
// by parent before reserving share access. Storage never enforces SMB sharing.
// Methods must not call back into state or the server while holding inode locks.
type Storage interface {
	// Lookup resolves a share-relative SMB name once. Empty path names the
	// root. It rejects traversal and stream syntax.
	// A missing final component returns Exists=false, not ErrNameNotFound;
	// missing ancestors return ErrPathNotFound. Attr is valid only when Exists.
	Lookup(ctx context.Context, path string) (Resolved, error)
	// Open takes an existing identity, never creates or truncates it. It returns
	// a handle whose Key is exactly object and whose data permissions are access.
	// AccessAppend restricts WriteAt even when AccessWrite permits initialization
	// by Truncate. Zero inode is an error.
	Open(ctx context.Context, object Inode, access Access) (Handle, error)
	// Create exclusively creates the named object. Existing objects return
	// ErrNameCollision. It returns the identity but does not open it.
	// Supersede is a server-controlled mutation.
	Create(ctx context.Context, name Name, kind Kind) (Resolved, error)
	// Close releases one storage reference exactly once. Flush errors propagate.
	Close(ctx context.Context, handle Handle) error
	// ReadAt reads the file. A short read returns its byte count and
	// io.EOF; the server sends bytes if n>0, or STATUS_END_OF_FILE if n==0.
	ReadAt(ctx context.Context, handle Handle, dst []byte, offset uint64) (int, error)
	// WriteAt writes the file. With AccessAppend it rejects offsets below the
	// file's current EOF atomically with every write and length change. Short
	// writes always return an error.
	WriteAt(ctx context.Context, handle Handle, src []byte, offset uint64) (int, error)
	// Flush covers all writes completed before the call on this inode, even
	// from other handles. Both modes return only after data is in S3 and local
	// metadata is durable. SyncFull does this for every inode. The server maps FLUSH Reserved1=0xffff to SyncFull and 0 to SyncData.
	Flush(ctx context.Context, handle Handle, mode SyncMode) error
	// Truncate changes the file's length coherently. Pending writes
	// cannot later resurrect removed bytes. Extensions read as zeroes.
	// AccessWrite is required; AccessAppend alone does not permit truncation.
	Truncate(ctx context.Context, handle Handle, size uint64) error
	// GetAttr returns the object's live size, including buffered data,
	// without a flush.
	GetAttr(ctx context.Context, object Inode) (Attr, error)
	// SetAttr applies changes in order with buffered writes on the inode. Size
	// changes use the Truncate contract. Explicit times survive subsequent flush.
	SetAttr(ctx context.Context, object Inode, change AttrChange) error
	// ReadDir returns at most limit entries with live attributes, starting at
	// cookie. At exhaustion it returns an empty slice and nil error. Cookies
	// remain usable without a storage cursor; the server owns filtering.
	ReadDir(ctx context.Context, inode Inode, cookie Cookie, limit uint32) ([]DirEntry, error)
	// Remove deletes exactly name if its inode is expect. Identity mismatch
	// leaves the namespace unchanged.
	Remove(ctx context.Context, name Name, expect Inode) error
	// Rename moves exactly the expected identities, atomically. It does not
	// rewrite handle paths; identities and open-table keys stay unchanged.
	Rename(ctx context.Context, request RenameRequest) error
	// PathOf returns the current share-relative path. No hard links are
	// supported. An unlinked inode or one with multiple paths is not found.
	PathOf(ctx context.Context, inode Inode) (string, error)
	// StatFS reports volume identity and configured capacity, independent of handles.
	StatFS(ctx context.Context) (Space, error)
}
