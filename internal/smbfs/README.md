# Native SMB filesystem adapter (#22)

`New(*fs.FileSystem, readOnly) (*FS, error)` consumes the bundled JuiceFS native filesystem. `FS` implements the pinned `vfs.VFSFileSystem` and `vfs.ByteRangeLocker`; `LockContext` additionally supports protocol request/connection cancellation. It does not own the native filesystem, metadata session, object store, or backup scheduler.

Initialize the volume root for native UID/GID **65534**, exported as `UID`/`GID`. Every client operation uses that stable nonroot identity even if the process runs as root. The share is the native filesystem root, not a host-directory passthrough. The application must enforce the documented single-writer restriction and install backup protection before starting native mutable work.

## Behavior and integration

- Paths are share-relative (`""`, `"."`, and `"/"` designate the root). Absolute nonroot paths, drive names, NULs, and explicit parent traversal are rejected. Native resolution checks every prefix, including symlinks, against native trash/internal inodes. Native special files such as `.config`/`.control` are not exposed, including through aliases.
- Handles contain native `*fs.File` objects and retain inode identity. Positional reads/writes do not use a shared cursor. Namespace operations serialize against handle I/O; unrelated file I/O may proceed concurrently. Rename updates descendant handle paths. Deleted/replaced paths cannot retarget a later handle unlink/rename onto a replacement file.
- Open handles into native trash cannot mutate it. A still-live hardlink outside trash retains normal native write behavior. Trash cleanup/reference retirement protection itself belongs to the native maintenance gate, not this adapter.
- Native `Setlk`/`Getlk` enforce ranges. Unique handle owners and native conflict queries cover aliases/hardlinks. A handle keeps only its acquired SMB ranges to translate exact SMB unlocks into native coalesced POSIX ranges; there is no separate conflict authority. Batch validation precedes mutation; errors restore the previous native projection. Closing a handle wakes lock waiters and releases all its locks. The narrow bundled SQL lock-only transaction exception allows locks on a readonly volume without permitting namespace/data writes or creating a mutable session.
- `Flush` and `FSync` call native `File.Fsync`; directory and read-only handles work. `Close` reports unlock/flush/close failures. Native `O_SYNC` writes synchronize before success. The **protocol handler** owns SMB `WRITE_THROUGH`/`FILE_WRITE_THROUGH`, including resource-fork writes, and propagates failures.
- `GetAttr` flushes buffered writes to the same inode before querying native metadata: native `File.Stat` is an open-time snapshot, so reporting it would return stale sizes. This can add I/O to attribute queries. Native access/modification/change timestamps, mode, ownership, links and xattrs are retained. JuiceFS has no independent birth-time field; the adapter retains the pinned server's fallback rather than inventing another attribute format. Setting birth time has the same unsupported/no-op behavior as the pinned passthrough adapter.
- Directory enumeration is a native-entry snapshot per handle; continuation does not duplicate entries when the namespace changes. Restart obtains a fresh snapshot. Trash is omitted.
- Read-only rejects all adapter data/namespace/xattr/attribute mutations with `EROFS`; storage must also instantiate its native readonly configuration. Errors remain native errno values for protocol status mapping. Empty/nil xattr buffers query size; short nonnil buffers return `ERANGE` instead of truncating data.

Stop accepting and drain protocol requests first, then call `Shutdown() error`, then close native resources. Shutdown rejects new work, wakes lock waiters, flushes/closes handles and returns joined failures. Like native I/O it is not a hard timeout; the application owns the bounded process shutdown policy. Do not release the state lock while native work remains active.

## Tests and evidence limits

Fast native integration tests use real SQLite metadata, native filesystem/cache/chunks, and native file-backed object storage. `TestTrashHandlePreservesExportedData` exports native metadata, deletes the live file, rejects destructive handle operations, imports the export into fresh SQLite and verifies the exact content. It is not an S3 backup or Time Machine proof. Object-upload fault tests wrap the real file store's `Put` to return `ENOSPC` and assert failed native sync/flush/close operations are not acknowledged.

Run with the shared memory gate:

```sh
flock /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p 2 -race ./internal/smbfs -count=1
```

The shared Docker runner can execute this same package; the test owner records Docker/MinIO and actual SMB evidence separately. Birth-time preservation, complete Mac metadata behavior, power-loss durability, and real Time Machine remain later release gates, not conclusions from these native fixtures.
