// Package smbfs adapts JuiceFS to smb.Storage. It owns path and stream resolution,
// identity-checked namespace changes and per-inode coherence on the shared writer.
// It must not own SMB opens, directory cursors, share modes, deletion intent,
// byte locks or leases, and must not call JuiceFS plocks. It never imports server.
//
// M1 provides New(options Options) (smb.Storage, error) and
// NewMetadataBarrier(metadataPath string) (MetadataBarrier, error). The latter
// covers the SQLite database and WAL with ordinary fsync or the platform's
// full-fsync barrier. It owns no persistent file descriptor or metadata connection.
// Tests may inject a barrier to check ordering and failures. There are no global
// handle or open registries. Handles carry a JuiceFS reference, immutable object
// key and access mode. Private per-inode coordination is allowed and must not
// serialize unrelated inode I/O. New rejects a missing filesystem or barrier.
// The caller owns and closes JuiceFS only after server shutdown closes all opens.
package smbfs

import (
	"context"

	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
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
	Filesystem *jfs.FileSystem
	Barrier    MetadataBarrier
	Capacity   uint64
	ReadOnly   bool
}
