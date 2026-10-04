// Package smbfs adapts JuiceFS to smb.Storage. It owns path and stream resolution,
// identity-checked namespace changes and per-inode coherence on the shared writer.
// It must not own SMB opens, directory cursors, share modes, deletion intent,
// byte locks or leases, and must not call JuiceFS plocks. It never imports server.
//
// M1 provides New(options Options) (*FS, error) and
// NewMetadataBarrier(metadataPath string) (MetadataBarrier, error). The latter
// covers the SQLite database and WAL with ordinary fsync or the platform's
// full-fsync barrier. It owns no persistent file descriptor or metadata connection.
// Tests may inject a barrier to check ordering and failures. There are no global
// handle or open registries. Handles retain an inode reference, immutable object
// key, kind and access mode. Private per-inode coordination is allowed and must not
// serialize unrelated inode I/O. New rejects a missing filesystem or barrier.
// After server shutdown drains requests and closes all opens, the caller calls
// FS.Shutdown to close its directory connection, then closes the JuiceFS runtime.
package smbfs

import (
	"context"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
)

// MetadataBarrier makes already committed metadata durable on local storage.
// Commit waits for the SQLite transaction/WAL and fsync ordering required by the
// storage runtime. With full=true it also waits for the platform full-fsync
// barrier. It must not create a metadata backup or claim machine-loss durability.
// The adapter calls it only after the inode's data reaches S3 and metadata commits.
// Barrier errors are flush failures, not successful acknowledgements.
type MetadataBarrier interface {
	Commit(ctx context.Context, full bool) error
}

// Options supplies the existing JuiceFS runtime, metadata barrier and capacity
// policy. Capacity zero means no configured quota; free space is capped at 1 TiB.
// ReadOnly forbids every mutation, including stream xattr writes.
type Options struct {
	// Config and Store are the runtime I/O settings and chunk store. The adapter
	// owns one shared JuiceFS reader/writer, separate from FileSystem's private I/O.
	Store      chunk.ChunkStore
	Barrier    MetadataBarrier
	Config     *vfs.Config
	Filesystem *jfs.FileSystem
	// MetadataPath names the runtime SQLite database. Directory pages use a
	// indexed queries on one long-lived connection because Meta has no stable
	// paged enumeration API.
	MetadataPath string
	Capacity     uint64
	ReadOnly     bool
}
