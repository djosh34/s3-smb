// Package storage is version 2 of the smallest storage interface that serves
// Time Machine. It holds only types and contracts, no implementation.
// DESIGN.md explains the choices. LAYOUT.md gives the bucket layout and the S3
// requests each operation costs.
//
// Two rules decide when anything reaches S3:
//   - Data changes (WriteAt, Truncate) stay in memory until Flush or Sync
//     returns. A crash may lose them, fully or partly, like a disk cache.
//   - Namespace changes (Create, Rename, Delete) are in S3 when they return.
//     A crash in the middle leaves every file whole at its old path, its new
//     path or both, never half a file.
//
// Every method that calls S3 retries a failing request for up to 5 minutes,
// then returns the error. Every S3 write first checks the bucket lock.
package storage

import (
	"context"
	"errors"
	"time"
)

// ChunkSize is the fixed size of every chunk except the last one of a file.
const ChunkSize = 8 << 20

// Paths are relative to the share root, separated by "/", with no leading
// or trailing slash. The root is "". Lookups are exact-case.

// Entry describes a file or directory as it is now, buffered writes
// included. Nothing in it is stored in S3:
//   - Size is where the last chunk ends.
//   - ModTime is the newest Last-Modified of the file's chunks, or of a
//     directory's marker. A chunk this server PUT since Open counts with the
//     local time the PUT returned, since a PUT response has no Last-Modified.
//
// The SMB file ID is a 64-bit FNV-1a hash of Path, with 0 mapped to 1. It
// changes on rename.
type Entry struct {
	ModTime time.Time
	Path    string
	Size    int64
	Dir     bool
}

var (
	// ErrNotFound reports a missing path or parent, or a File that was
	// deleted.
	ErrNotFound = errors.New("storage: not found")
	// ErrExists reports a target path that is already taken.
	ErrExists = errors.New("storage: already exists")
	// ErrNotEmpty reports Delete of a directory that has entries.
	ErrNotEmpty = errors.New("storage: directory not empty")
	// ErrInvalid reports a bad path (an empty, "." or ".." segment, or a key
	// over S3's 1024 bytes), Rename or Delete of the root, a directory
	// renamed into itself, List on a file, or a data method on a directory.
	ErrInvalid = errors.New("storage: invalid argument")
	// ErrCorrupt reports a file whose chunks have a gap or a wrong length.
	// Its ReadAt, WriteAt, Truncate, Flush and Rename return it. Stat and
	// Delete still work, so the file can be removed.
	ErrCorrupt = errors.New("storage: corrupt file")
	// ErrLockLost means the bucket lock was not renewed for 8 minutes.
	// Nothing was written. The server must exit.
	ErrLockLost = errors.New("storage: bucket lock lost")
)

// Storage is the namespace. The engine provides
//
//	func Open(ctx context.Context, s3 S3, serverID string) (Storage, error)
//
// Open claims the bucket lock, LISTs the whole bucket into memory, deletes
// leftover chunks (those without a chunk 0) and checks every file's shape.
// It starts renewing the lock every minute until Close. serverID is the
// hostname plus the bucket name.
//
// This server is the only writer, so the namespace in memory is the truth.
// Lookup, Stat and List never call S3. All methods are safe for concurrent
// use.
type Storage interface {
	// Lookup returns the file or directory at path, or ErrNotFound.
	// Lookup("") returns the root.
	Lookup(path string) (File, error)

	// Create makes an empty file or directory at path and returns it. The
	// parent must exist (ErrNotFound) and path must not (ErrExists). For a
	// file it first deletes leftover chunks under path, then PUTs an empty
	// chunk 0. For a directory it PUTs a marker.
	Create(ctx context.Context, path string, dir bool) (File, error)

	// Sync is Flush for every file at once (SMB FLUSH with SyncFull). It
	// covers every WriteAt and Truncate that returned before the call.
	Sync(ctx context.Context) error

	// Close runs Sync, stops renewing the lock and deletes this server's
	// lock key. Nothing may be called after Close.
	Close(ctx context.Context) error
}

// File is one file or directory. It is not an open: the SMB server keeps
// opens, share modes, leases and durable handles, and many opens share one
// File. So there is no Open and no Close.
//
// A File stays the same file across Rename, also when an ancestor directory
// is renamed. One file is always the same File value, so a File can key a
// map. After Delete every method returns ErrNotFound, and a file created
// later at the same path is a different File.
type File interface {
	// Stat returns the entry as it is now, from memory.
	Stat() (Entry, error)

	// List returns the entries of a directory, sorted by path, from memory.
	List() ([]Entry, error)

	// ReadAt reads into p at off, buffered writes included. It reads chunks
	// that are not in memory with ranged GETs. Reading past the end returns
	// a short count and io.EOF. A chunk missing from S3 is ErrCorrupt.
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)

	// WriteAt writes p at off into memory. A write past the end fills the
	// skipped range with real zeros. It may GET a chunk it only partly
	// overwrites. Under memory pressure it may upload this file's dirty
	// chunks early, the same way Flush does.
	WriteAt(ctx context.Context, p []byte, off int64) error

	// Truncate sets the size, for SMB SET_INFO EndOfFile and CREATE with
	// overwrite. Growing adds real zeros, shrinking drops the bytes past
	// size. It changes memory only, and the same size is a no-op. Dropped
	// bytes never come back after a later Flush.
	Truncate(ctx context.Context, size int64) error

	// Flush makes every WriteAt and Truncate on this file that returned
	// before the call durable (SMB FLUSH). It DELETEs chunks above the new
	// end from the top down, PUTs dirty chunks below the old end in
	// parallel, then PUTs new chunks upward one at a time, so a crash never
	// leaves a gap. It returns once S3 has all of them.
	Flush(ctx context.Context) error

	// Rename moves the file or directory tree to path. The parent of path
	// must exist (ErrNotFound) and path must not (ErrExists). It flushes the
	// files it moves, COPYs every chunk with each chunk 0 last and a
	// bundle's Info.plist the very last, then DELETEs the old objects with
	// each chunk 0 first. Calls on the moved files wait until it returns,
	// and Lookup sees the old path until then.
	Rename(ctx context.Context, path string) error

	// Delete removes a file, or a directory if it is empty (ErrNotEmpty).
	// For a file it drops the dirty chunks, DELETEs chunk 0, then the rest.
	Delete(ctx context.Context) error
}

// Object is one key as LIST returns it.
type Object struct {
	LastModified time.Time
	Key          string
	Size         int64
}

// S3 is the only part of S3 the engine uses. Each call is one atomic
// request. The backend must have strong read-after-write and
// list-after-write consistency. No multipart, no conditional writes and no
// metadata are used.
type S3 interface {
	// Put stores body at key, replacing any old object.
	Put(ctx context.Context, key string, body []byte) error

	// Get reads n bytes at off from key (a ranged GET). It returns
	// ErrNotFound if the key does not exist.
	Get(ctx context.Context, key string, off, n int64) ([]byte, error)

	// Delete removes key. A missing key is not an error.
	Delete(ctx context.Context, key string) error

	// Copy copies from to to inside the bucket, server-side.
	Copy(ctx context.Context, from, to string) error

	// List calls fn for every key under prefix in key order, reading all
	// pages.
	List(ctx context.Context, prefix string, fn func(Object) error) error
}
