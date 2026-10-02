# Native storage integration

This package embeds the pinned JuiceFS `meta.Meta`, `object.ObjectStorage`,
`chunk.ChunkStore`, and `*fs.FileSystem`; it does not implement another filesystem,
cache, or export format.

## Lifecycle boundaries

`OpenS3` snapshots resolved credentials and TLS into an independently owned HTTP
transport; it does not create a bucket or discover ambient credentials.
`ReadIdentity` and `DiscoverRecoveryVolume` are read-only. Only an explicitly
confirmed, verified-empty dataset may call `OpenVolume(..., create=true)`.
`OpenMetadata` constructs SQLite through native `meta.NewSQLite`; effective FULL
synchronization is enforced/tested by the native metadata owner.
`OpenFilesystem` constructs native cache/filesystem and registers native
DeleteSlice/CompactChunk callbacks. It does **not** initialize metadata or start a
session. Lifecycle must establish successful protection before starting mutable
native workers or serving SMB.

Shutdown order: stop accepting SMB; flush and close every adapter handle; stop
and join backup; `Runtime.Close` (native filesystem Close, including CloseSession);
metadata Shutdown; raw S3 Close; state-lock release. The application owns the
shutdown deadline. Cache writeback is disabled. Harmless native read/cache loops
remain process-lifetime work; metadata mutator shutdown belongs to native meta.

## Remote layout and keys

- `s3-smb/format.json`: saved native Format JSON, with S3 credentials/destination
  removed. Current validated YAML remains the S3/TLS authority.
- `s3-smb/keys/<native UUID>.pem`: passphrase-protected RSA-3072 key, outside native
  chunk/metadata-backup cleanup. Losing this object prevents encrypted recovery.
- `<native Name>/juicefs_uuid`, `chunks/…`, and native metadata backup paths:
  ordinary native prefix and encryption wrapper. The application uses Name
  `s3-smb`.

The bootstrap profile is standard encrypted PKCS#8 PBES2 with AES-256-GCM
(12-byte nonce, 16-byte authentication tag), scrypt N=131072, r=8, p=1, and a
32-byte random salt. This costs 134.217728 MB of principal scrypt working memory.
Use a high-entropy passphrase. Inputs are bounded to 32,768 bytes and ASN.1
algorithm/cost/nonce parameters must match this profile **before** native parser
password derivation. Plaintext/legacy/CBC keys are not accepted. This is not a new
cryptographic encoding or a password-derived RSA key.

Publication uses S3 `If-None-Match: *`, including through native prefix/encryption
wrappers, and verifies exact readback. A lost PUT response with matching readback
is safe; a different existing object is never replaced. Existing missing/corrupt
keys never trigger recovery-time generation. Disabled encryption never generates,
fetches, or unlocks a key. Native plaintext SQLite/WAL/cache/export staging are not
encrypted by object encryption.

A missing format marker does not authorize initialization. Recovery may discover
one existing protected key, unlock a candidate volume, and inspect a native
backup for authoritative identity. Lifecycle must compare the inspected UUID and
mode before importing, and require explicit confirmation of the selected point.

## Cache and transport

Native `chunk.Config.CacheSize` is bytes. Decimal configured bytes pass through
exactly, including a positive one-byte capacity. Omission retains the pinned
native default (107.3741824 GB); explicit zero runs native SelfCheck and disables
both retained caches and prefetch/writeback, ignoring any old/unusable cache path.
Ordinary I/O buffers remain (native default 314.5728 MB), not a retained data cache.

S3 path style is passed explicitly when configured, including false. HTTPS
verification cannot be disabled; only explicit `http://` is HTTP. Transport TLS
roots/client certificates are a startup snapshot and never modify OS trust.

## Tests and evidence boundaries

`GOMAXPROCS=2 go test -p 2 ./internal/storage` includes bounded crypto/native parser,
publication-fault, native object encryption, decimal/zero cache, cold native
file-backed reads, failed uploads, and final deletion-guard regressions. Fixture
publication faults are **not** proof of real S3 lost-response behavior. The native
file-backed zero tests are **not** MinIO/SMB acceptance.

`TestMinIOBootstrapLostResponse` needs MinIO and runs in Docker
(`scripts/test-linux.sh ./internal/storage`): a proxy closes the
connection after a real MinIO key PUT succeeds. Production bootstrap must resolve
exact readback without replacing the key. It also tests that MinIO refuses to
overwrite the key and that a reopened volume decrypts. This does not simulate a
partially received upload body or host power failure.

The transport test and the SMB-to-MinIO tests also run in Docker. No
power-loss or Mac acceptance is implied by these tests.
