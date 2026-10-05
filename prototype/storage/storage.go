// Package storage is a prototype of the smallest storage interface that
// serves Time Machine. It holds no implementation. See LAYOUT.md for the
// bucket layout and the S3 requests each operation costs.
package storage

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"time"
)

// ChunkSize is the fixed size of every chunk except the last one of a file.
const ChunkSize = 8 << 20

// Paths are relative to the share root, separated by "/", with no leading
// or trailing slash. The root is "".

// Entry describes a file or directory. Nothing in it is stored: a file's
// size is where its last chunk ends, and its modified time is the newest
// Last-Modified of its chunks. A directory's time is its marker's.
type Entry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

var (
	ErrNotFound = errors.New("storage: not found")
	ErrExists   = errors.New("storage: already exists")
	ErrNotEmpty = errors.New("storage: directory not empty")
	// ErrCorrupt reports a file whose chunks have a gap or a wrong length.
	ErrCorrupt = errors.New("storage: corrupt file")
	// ErrLockLost means the bucket lock was not renewed for 8 minutes.
	// Nothing was written. The server must exit.
	ErrLockLost = errors.New("storage: bucket lock lost")
)

// Storage is everything the SMB server asks of storage. The namespace is
// held in memory and this server is the only writer, so Stat and List never
// call S3. Every method that writes to S3 first checks the bucket lock.
type Storage interface {
	// Stat returns the entry at path, or ErrNotFound.
	Stat(ctx context.Context, path string) (Entry, error)

	// List returns the entries of the directory at path, sorted by name.
	List(ctx context.Context, path string) ([]Entry, error)

	// Create makes an empty file or directory. The parent must exist and
	// path must not. For a file it first deletes leftover chunks under the
	// path, then PUTs an empty chunk 0. It returns once S3 has it.
	Create(ctx context.Context, path string, dir bool) error

	// ReadAt reads from the file, dirty data included. Reading past the end
	// returns io.EOF.
	ReadAt(ctx context.Context, path string, p []byte, off int64) (int, error)

	// WriteAt writes into memory and marks chunks dirty. A write past the
	// end fills the skipped range with real zeros. It may GET a chunk to
	// patch it, and may upload dirty chunks early under memory pressure.
	// Nothing written is durable until Flush returns.
	WriteAt(ctx context.Context, path string, p []byte, off int64) error

	// SetSize changes the file size. Growing is a write of zeros. Shrinking
	// deletes the chunks above the new end from the top down, then PUTs the
	// shortened last chunk, and returns once both are done.
	SetSize(ctx context.Context, path string, size int64) error

	// Flush uploads dirty chunks and returns once every PUT succeeded,
	// retrying within the 5 minute outage window. With all false it covers
	// the file at path. With all true (SyncFull) it covers every file.
	Flush(ctx context.Context, path string, all bool) error

	// Delete removes a file (chunk 0 first, then the rest) or an empty
	// directory. Dirty chunks of the file are dropped.
	Delete(ctx context.Context, path string) error

	// Rename moves a file or a directory tree to a target that does not
	// exist (ErrExists otherwise). It flushes the source first, copies with
	// every chunk 0 last (a bundle's Info.plist the very last), then
	// deletes the old objects with every chunk 0 first.
	Rename(ctx context.Context, from, to string) error

	// Close flushes everything and deletes this server's lock key.
	Close(ctx context.Context) error
}

// Open claims the bucket lock, LISTs the whole bucket into memory, removes
// leftover chunks (those without a chunk 0) and checks every file's shape.
// It starts renewing the lock every minute until Close. serverID is the
// hostname plus the bucket name.
func Open(ctx context.Context, s3 S3, serverID string) (Storage, error) {
	return nil, errors.New("storage: prototype, not implemented")
}

// FileID is the SMB file ID of path: a 64-bit hash, with 0 mapped to 1.
// It is stable across restarts and changes on rename.
func FileID(path string) uint64 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, path)
	if id := h.Sum64(); id != 0 {
		return id
	}
	return 1
}

// Object is one key as LIST returns it.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
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
