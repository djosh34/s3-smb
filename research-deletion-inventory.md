# What the new design deletes from v0.2.0

Research for [#544](https://github.com/djosh34/s3-smb/issues/544), under the map [#173](https://github.com/djosh34/s3-smb/issues/173).

## Method

- Source: `main` at `a226608`, read-only. Line counts are `wc -l` of tracked files, tests included.
- Decisions: the map body of #173, and the storage interface in #542.
- Time Machine facts: `research-tm-mac-trace.md` on `research/tm-mac-trace` (#528, #535).
- `internal/smb-old` (26,824 lines) and the `server_old*` files in `internal/app` are not counted. [#489](https://github.com/djosh34/s3-smb/issues/489) deletes them before v0.2.0, so v0.2.0 does not have them.
- "Est." marks a line count for part of a file. It is a rough guess from grep, not an exact count.
- Actions: **delete** (gone with nothing in its place), **replace** (new, smaller code takes its place), **simplify** (stays, gets smaller).

## Inventory

### JuiceFS world

| # | Path | Lines | Why it exists | Action | Tag |
|---|---|---:|---|---|---|
| 1 | `internal/juicefs` (vendored JuiceFS v1.4.1: meta, chunk, object, vfs, utils, compress, acl, fs, version) | 37,513 | The filesystem engine: SQLite metadata, chunk store, cache, compression, encryption | delete | JuiceFS |
| 2 | `internal/thirdparty/xorm` | 19,847 | SQL ORM that JuiceFS meta uses for SQLite | delete | JuiceFS |
| 3 | `internal/thirdparty/mpb` | 3,242 | Progress bars that JuiceFS utils import | delete | JuiceFS |
| | `internal/thirdparty` total | 23,089 | | | |
| 4 | `internal/smbfs` (fs, namespace, attributes, directory, barrier, read_retry, tests) | 4,112 | Adapter from the 16-method `smb.Storage` to JuiceFS vfs. Streams map to xattrs. | replace by the new engine (#542) | JuiceFS |
| 5 | `internal/storage` (volume, keys, runtime, MinIO and transport tests) | 2,085 | Builds the JuiceFS volume, object keys, format, cache dirs and the S3 client for JuiceFS | delete. The new engine owns its S3 client and the 5-method S3 interface | JuiceFS |
| 6 | `internal/backup` (snapshot, retention, recovery, protection, cache, validation, tests) | 3,350 | Metadata DB snapshots to S3, trash retention, recovery from a snapshot, delete protection | delete. No database, no snapshots, no trash (#507). Recovery is a new server on the bucket (#508) | JuiceFS |
| 7 | `internal/app/serve.go` | 566 | Opens the state dir, SQLite DB, volume discovery, recovery prompt, backup manager, then the SMB server | simplify to config, bucket lock, LIST, serve, shutdown. Est. about 150 left | JuiceFS |
| 8 | `internal/app/lock.go` | 47 | flock on the local state dir, so two processes cannot share one DB | replace by the bucket lock key (#519) | JuiceFS |
| 9 | `internal/app/prompt.go` | 34 | TTY confirm for "initialize empty dataset" and "recover from metadata point" | delete | JuiceFS |
| 10 | `internal/app/startup_test.go`, parts of `shutdown_test.go` | est. 250 | Test DB, recovery and backup startup and shutdown order | replace with tests of the new startup | JuiceFS |
| 11 | `internal/config` keys `encryption.*` (passphrase, enabled), `backup.*` (interval, trash_days), `storage.cache_size`, `storage.cache_dir`, `storage.state_dir` | est. 150 of 1,295 | Configure JuiceFS encryption, metadata backup, trash, cache and the local DB | delete. Add `storage.memory_chunks` (#540) | JuiceFS |
| 12 | `internal/logging/native.go` and its tests | 61 + est. 40 | Routes JuiceFS logrus logs into slog | delete. Drops logrus | JuiceFS |
| 13 | `go.mod` / `go.sum` | 86 / 259 | 41 direct requires. Only about 11 are used outside the JuiceFS packages: aws-sdk-go-v2 (core, config, credentials, s3, smithy), go-smb2 (tests), creack/pty, x/crypto, x/sys, yaml.v3, plist | simplify. About 30 direct and most of 36 indirect requires drop, among them zstd, lz4, sqlite3, go-redis, goleveldb, sftp, prometheus, xorm.io/builder, gmsm, logrus, msgpack, protobuf | JuiceFS |
| 14 | `.golangci.yml` | est. 8 of 99 | `contextcheck` exclusion for serve.go and backup/recovery.go, `G204` path for backup tests, `old-code` paths for juicefs and thirdparty | delete those entries. Drop the `old-code` anchor once smb-old is also gone | JuiceFS |
| 15 | `test/e2e`: `backup_outage`, `cache_recovery`, `cold_recovery`, `protection`, `uncompressed`, `namespace_measurement` | 787 | Metadata backup under S3 outage, cache recovery, cold recovery from snapshots, failing backups, no-compression format, snapshot size measurements | delete | JuiceFS |
| 16 | `test/e2e/startup_failure_test.go` | 567 | Compressed format, partial remote state, broken recovery points, identity-missing recovery | replace by short tests for the bucket lock and startup LIST cleanup (#524). Est. about 120 left | JuiceFS |
| 17 | `test/e2e/recovery_test.go`, `capacity_test.go`, `fixture_test.go` | 128 + 97 + 595 | Recovery twice from S3 with no local state, capacity on recovery, fixture with state dirs and metadata points | simplify. Recovery becomes "new server, same bucket". Est. -300 | JuiceFS |
| 18 | `test/macos/scenario_test.go` and `harness_test.go` metadata parts | est. 150 of 739 | Wait for metadata points before kills, check `recovered_from`, the `recover` daemon start | simplify. Kill scenarios stay, metadata waits go | JuiceFS |
| 19 | `.github/workflows/macos.yml`: SQLite full-fsync step, `machine-loss` store upload and its "machine-loss on a fresh Mac" job | est. 80 of 334 | SQLite F_FULLFSYNC check, and recovery from a metadata snapshot on a fresh Mac | delete the SQLite step. Fold machine-loss into the plain "recovery on a fresh Mac" job, which then only points a new server at the bucket | JuiceFS |
| 20 | `scripts/check.sh`, `test/check_test.sh` | est. 4 | JuiceFS and old-code mentions | delete those lines | JuiceFS |
| 21 | `docs/vendored.md` | 135 | Lists every change to vendored JuiceFS, xorm and mpb | delete | JuiceFS |
| 22 | `docs/recovery.md` | 149 | Metadata points, recovery prompt, staging, trash | replace with a short page: point a new server at the bucket, or `cat` chunks by hand (#529). Est. 40 | JuiceFS |
| 23 | `docs/configuration.md` sections Retention, Encryption, cache and state keys, defaults rows | est. 70 of 208 | Document the JuiceFS keys | delete those parts. Add `memory_chunks`, and "Encrypt backups" as step one (#514) | JuiceFS |
| 24 | `docs/development.md` sections Startup, Delete protection, vendored lint notes | est. 80 of 313 | Explain the DB, recovery and backup startup order and the vendored code | simplify | JuiceFS |
| 25 | `README.md` sections "What is stored and how recovery works", encryption, cache limits | est. 40 of 169 | User docs for the JuiceFS bucket format and recovery | simplify | JuiceFS |
| 26 | `NOTICE` JuiceFS, mpb and xorm entries | est. 10 of 44 | Licence notices for the vendored code | delete those entries | JuiceFS |

### SMB server

The SMB column says what Time Machine (TM) and MS-SMB2 need, from the traces.

| # | Path | Lines | Why it exists | TM / spec need | Action | Tag |
|---|---|---:|---|---|---|---|
| 27 | Named streams: `server/streams.go`, `streams_test.go`, `stream_fuzz_test.go`, stream parts of `query_info.go`, `set_info_namespace.go`, `state/opens.go`, `state/delete_test.go`, `wire/info*.go` (FileStreamInformation), `ObjectKey.Stream`, `StreamInfo`, `Streams()`, `MaxStreamSize`, `FileNamedStreams` | est. 600 | macOS xattrs, Finder info and the AAPL empty-stream rule | TM wrote no stream and no xattr in any run. With streams off, macOS sends no stream requests (#535). Spec: optional | delete (#530). Stream opens return not found | SMB |
| 28 | `test/macos/features_test.go`, `TestResourceForkOffsetsAndResize` in `test/e2e/operations_test.go` | 49 + est. 40 | Check xattrs, FinderInfo and resource forks on the share | Not used by TM | delete | SMB |
| 29 | Byte-range locks: `server/lock.go`, `lock_test.go`, `state/locks.go`, `locks_test.go`, lock wire codec, lock refs in `state/opens.go` and `durable.go` | est. 500 | MS-SMB2 LOCK, conflicts with READ and WRITE, auto-unlock on close | 0 LOCK requests in every run. smbfs sends LOCK only for flock and fcntl locks. Spec allows NOT_SUPPORTED | delete (#531). A few lines answer LOCK with NOT_SUPPORTED | SMB |
| 30 | `test/e2e/smbtorture.allowlist` lock entries: 13 `smb2.lock.*` plus `smb2.create.brlocked` | 14 of 65 | smbtorture coverage of locks | Follows #29 | delete those lines | SMB |
| 31 | `internal/smb/storage.go` (`smb.Storage`, 16 methods, Inode, ObjectKey, Name, Resolved, Handle, Cookie, AttrChange, PathOf) | 227 | Seam to the JuiceFS adapter | Not kept (#505) | replace by Storage (Lookup, Create, Sync, Close) and File (Stat, List, ReadAt, WriteAt, Truncate, Flush, Rename, Delete) from #542. Est. about 100 | SMB |
| 32 | `internal/smb/smbtest/storage.go` | 138 | Builds a real JuiceFS adapter with SQLite for SMB tests | Test helper | replace with the new engine on a fake S3 | SMB |
| 33 | `server/fixture_test.go` and tests that drive `smb.Storage` directly | 783 + parts | Fake storage and helpers for the old seam | Test helper | simplify to the new interface. Est. -300 | SMB |
| 34 | `server/namespace.go` parent guards and `lockName`, `PathOf` lookups in `set_info_namespace.go` | 177 + 155 | Per-inode parent locks around create, rename and delete, and path lookup from inode | File references follow renames and storage owns crash order (#542). File IDs are path hashes (#526) | simplify. Est. -150 | SMB |
| 35 | SET_INFO FileBasicInformation, `SetAttr`, `AttrChange` in `set_info.go`, `file_attributes.go` | est. 100 | Set times and attributes | TM set no times in any run (#528). Times come from S3 (#522). Spec: SET_INFO Basic must get a reply | simplify. Accept and ignore, or return a fixed status. No storage call | SMB |
| 36 | AAPL: `server/aapl.go`, AAPL codec in `wire/contexts*` | 44 + est. 80 | Tells macOS full sync and case sensitivity | Needed. `kAAPL_SUPPORTS_FULL_SYNC` gates TM's `kSMBFullFSyncSupported` | keep. Only the stream rule in #27 goes | SMB |
| 37 | Share modes and delete-pending: `state/opens.go`, `state/state.go`, `server/delete.go`, `sharing_test.go`, `delete_test.go` | est. 400 | MS-SMB2 and MS-FSA share access and delete-on-close semantics | Needed. TM deletes by disposition (16 per backup), opens for DELETE access, and smbfs uses deny modes for `O_EXLOCK`. Spec requires them | keep. Only stream keys go | SMB |
| 38 | Leases, durable v2, reconnect, scavenger: `create_lease.go`, `lease_break.go`, `durable.go`, `state/leases.go`, `state/durable.go`, `scavenger.go` | est. 1,100 | Leasing and durable handles | Needed. 125 durable grants per backup, one lease break per incremental. TM's durable v2 probe gates TM | keep. Lock fields go with #29 | SMB |
| 39 | Security info, CHANGE_NOTIFY, IOCTL | few lines | Return NOT_SUPPORTED | 29 security queries, 4 CHANGE_NOTIFY, 4 resume-key IOCTLs, all refused, and backups passed | keep as they are. Nothing to delete | SMB |
| 40 | `S3OutageWindow` in `features.go`, `smbfs/read_retry.go` users | est. 10 | Read retry window over JuiceFS | Owned by the engine now | move into the engine | SMB |

## What stays and changes

- `internal/smb/wire`, `crypt`, `auth`: stay. Only stream and lock codecs go.
- `internal/smb/server`: stays. Handlers call the #542 File reference instead of `smb.Storage` and inodes. File IDs become path hashes (#526).
- `internal/smb/state`: stays for opens, leases, durable handles, share modes and delete-pending. Locks and stream keys go.
- `smb.Storage` (`internal/smb/storage.go`): replaced by the new Storage and File interfaces from #542.
- FLUSH: a full FLUSH becomes a global barrier through `Storage.Sync` (#532). Today it flushes one handle.
- `internal/app`: stays, but serve shrinks to config, bucket lock, startup LIST and serve. No state dir, no DB, no prompt.
- `internal/config`: stays. JuiceFS keys go, `storage.memory_chunks` comes in. `storage.capacity`, S3 and TLS keys stay.
- `internal/s3fault`, `internal/netfault`: stay. They become the fault-injecting fake S3 and network tests the new design needs.
- New: the engine package (chunks, in-memory namespace, the 5-method S3 interface, bucket lock). Its size is not counted here.
- Chaos tests (`chaos_*`), `samba_test.go`, the Mac backup, network-drop and kill scenarios: stay, with metadata waits removed.
- CI: `check.yml` and `minio.yml` stay. The MinIO backend may move to Garage, which is a separate open question in #173.

## Rough total

| Group | Lines removed |
|---|---:|
| Vendored JuiceFS, xorm and mpb | 60,602 |
| `internal/smbfs`, `internal/storage`, `internal/backup` | 9,547 |
| App, config, logging | ~850 |
| Tests (e2e and Mac) | ~1,600 |
| Docs, NOTICE, CI, lint, scripts | ~570 |
| go.mod and go.sum | ~260 |
| **JuiceFS subtotal** | **~73,400** |
| Streams and locks (code, tests, allowlist) | ~1,200 |
| `smb.Storage` seam, test storage, fixtures, namespace guards, SetAttr | ~700 |
| **SMB subtotal** | **~1,900** |
| **Total** | **~75,000** |

- About 81% of the removal is vendored code.
- The total does not subtract the new engine and its tests.
- The tracked tree outside `internal/smb-old` is 109,615 lines, so the cut is about two thirds of it.
