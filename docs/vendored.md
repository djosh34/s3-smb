# Vendored source

s3-smb patches JuiceFS, the SMB server and two libraries that JuiceFS itself uses in patched form. `go install module@version` ignores `replace` directives, so `go.mod` cannot point at patched forks. The patched code is copied into `internal/` as ordinary packages of this module.

| Directory | Upstream | Version or commit | Licence file |
| --- | --- | --- | --- |
| `internal/juicefs` | github.com/juicedata/juicefs | v1.4.1, `0b90c7db5a929ae6adc5faad948d108efd2c99f9` | `internal/juicefs/LICENSE` |
| `internal/smb-old/smb2` | github.com/macos-fuse-t/go-smb2 | `277a9300411249a881a05f7a910f5a83ae3395f2` | `internal/smb-old/smb2/LICENSE`, `internal/smb-old/smb2/Attributions.txt` |
| `internal/thirdparty/xorm` | gitea.com/davies/xorm | `v1.0.8-0.20220528043536-552d84d1b34a` | `internal/thirdparty/xorm/LICENSE` |
| `internal/thirdparty/mpb` | github.com/juicedata/mpb/v7 | `v7.0.4-0.20231024073412-2b8d31be510b` | `internal/thirdparty/mpb/UNLICENSE` |

Only the packages the binary needs were copied. SQLite is the only metadata engine and S3, local file and memory are the only object stores. Files for Windows and other unsupported platforms are left out.

## What was changed

All import paths were rewritten to `github.com/djosh34/s3-smb/internal/...`. Every upstream file with any other change is listed below and carries the line `// Modified for s3-smb, 2026. See docs/vendored.md.`

### JuiceFS

| File | Change and reason | Test |
| --- | --- | --- |
| `pkg/fs/fs.go` | `pread` flushes the inode's writer and refetches the length before the end-of-file check. A second open handle saw a stale length after another handle wrote. | `internal/smb-old/smbfs/coherence_test.go` |
| `pkg/chunk/disk_cache.go` | The two cache-full flags are `atomic.Bool`. The free-space monitor raced with cache reads and writes. | `test/e2e` under `-race` |
| `pkg/utils/utils.go`, `pkg/chunk/cached_store.go` | Backport the upstream [timeout result fix](https://github.com/juicedata/juicefs/pull/7503) and [GET result fix](https://github.com/juicedata/juicefs/pull/7500) for the [WithTimeout result race](https://github.com/djosh34/s3-smb/issues/110). `WithTimeout` sends the callback error through a buffered channel. GET callbacks keep their byte count and request attributes private until success, and never write the caller's return error. Include the one-line [range-read timeout fix](https://github.com/juicedata/juicefs/pull/7509) so a timed-out range read cannot start a full read while the first callback still writes the page. | `pkg/utils/timeout_s3smb_test.go`, `pkg/chunk/timeout_s3smb_test.go` under `-race`. Both fail without their fixes. Tests also cover successful reads, GET errors, cancellation, timeout and ordinary range-read fallback. |
| `pkg/chunk/cached_store.go` | Apply only the cache publication fix from swarm commit `0db3360`, also proposed upstream as the [cache publication fix](https://github.com/juicedata/juicefs/pull/7346), for the [cache upload and flush race](https://github.com/djosh34/s3-smb/issues/128). Finish compression and the page slice-header write before handing the page to the asynchronous disk cache. The zstd change from that commit is not included. | `pkg/chunk/cache_publication_s3smb_test.go`: `TestCachedStoreUploadPublication` fails under `-race` without the fix and passes with it. It checks that a partial-block write reaches disk and reads back unchanged. |
| `pkg/chunk/disk_cache.go` | Apply swarm commit `f024cc84` for the [disk-cache scan race](https://github.com/djosh34/s3-smb/issues/144). Read the scan-complete flag under the same cache mutex used by startup scans and rescans, so the staging upload check cannot race with either scan. | `pkg/chunk/disk_cache_scan_s3smb_test.go`: `TestDiskCacheScanWithStagingCheck` fails under `-race` without the fix and passes with it. Both scan modes keep the cached block and its content. |
| `pkg/chunk/disk_cache.go`, `mem_cache.go`, `cached_store.go`, `pkg/meta/quota.go`, `sql.go` | Log lines print decimal units. | `pkg/chunk/logging_s3smb_test.go` |
| `pkg/meta/config.go` | New `Config.CheckMaintenance` hook. The application uses it to block slice deletion and compaction while a metadata backup still needs the older data. | `pkg/meta/protection_test.go`, `trash_protection_test.go`, `test/e2e/protection_test.go` |
| `pkg/meta/sql.go`, `base.go` | Transactions that delete, truncate or compact call the hook, through `maintenanceTxn`. So do `compactChunk`, `deleteSlice_` and trash cleanup. | same |
| `pkg/meta/base.go` | `refresh` and background deletes and compactions join the session wait group. `CloseSession` waits for them before SQLite closes. | `pkg/meta/protection_test.go` |
| `pkg/meta/sql.go` | The SQLite connection string forces `_synchronous=FULL`, so local metadata survives a crash. | `pkg/meta/protection_test.go`, `sqlite_path_test.go`, `sqlite_io_test.go` |
| `pkg/meta/sql_sqlite.go` | A SQLite driver connection hook sets `fullfsync=ON` and `checkpoint_fullfsync=ON` on every connection. macOS uses `F_FULLFSYNC` to flush the drive cache. The flags have no effect on other platforms. The driver has no DSN options for these flags, and JuiceFS exposes no connection hook. The engine name, dialect and existing DSN settings stay the same. | `pkg/meta/sqlite_fullfsync_test.go`, also run by the Mac acceptance backup job |
| `pkg/meta/sql.go`, `sql_lock.go` | `txn` is split so that flock and plock go through `lockTxn`. A read-only mount can still take advisory locks. | `pkg/meta/orphan_locks_test.go`, `internal/smb-old/smbfs/readonly_test.go`, `locks_test.go` |
| `pkg/meta/sql.go` | `DumpMeta` takes a mutex, always exports in one transaction on SQLite, returns directory errors, and no longer prints the payload on panic. A failed export is not reported as success. | `pkg/meta/protection_test.go` |
| `pkg/vfs/backup.go` | The periodic backup calls the new `BackupTo`, which stages the dump, syncs it, uploads it only if the key is absent, and reads it back. It sets the timestamp only on success. New `WriteBackup` and `CleanupBackups`. | `pkg/vfs/backup_smb_test.go`, `internal/backup/backup_test.go`, `test/e2e/recovery_test.go` |
| `pkg/object/s3.go` | `GetObject` maps `NoSuchKey` to `os.ErrNotExist`, so startup can tell an empty bucket from a failed login. The AWS SDK logs through the application logger. | `internal/storage/s3_test.go` |
| `pkg/utils/logger.go`, `logger_syslog.go` | Logrus output goes to the application logger. The syslog hook is removed. | `pkg/utils/logging_s3smb_test.go` |
| `pkg/utils/progress.go` | `NewProgress` never draws bars. | same |
| `pkg/utils/utils_linux.go` | `println` replaced by the logger. | none |
| `pkg/utils/humanize.go` | `ParseBytes` and `ParseMbps` deleted. They were the only users of the vendored `cli` library, which is gone. | build |

Added files: `pkg/meta/protection.go` (`NewSQLite`, `ClearOrphanLocks`, the maintenance hook), `pkg/object/s3_smb.go` (`NewS3` with explicit credentials, `PutIfAbsent`) and `pkg/object/publication_smb.go` (`PutIfAbsent` for the prefix and encryption wrappers).

Deleted: `pkg/sync`, `pkg/fs/http.go`.

### SMB server

| File | Change and reason | Test |
| --- | --- | --- |
| `internal/smb-old/smb2/internal/smb2/request.go` | CREATE and WRITE bounds checks use widened arithmetic. `WriteRequestDecoder.Data` uses the real data offset. A malformed packet cannot index out of range. | `server/malformed_test.go`, `error_wire_test.go` |
| `server/compound.go` | New `splitRequests` validates the whole compound chain before any request runs. | `server/compound_wire_test.go` |
| `server/conn.go` | A failed decrypt, a failed signature check or an unknown session closes the connection. Upstream skipped the packet and continued, so unsigned requests ran. Each part of a compound is verified. Queue sends can be cancelled. | `server/auth_wire_test.go`, `session_gate_test.go`, `cleanup_test.go` |
| `server/conn.go` | An SMB1 packet reaches the SMB2 upgrade handler only as the first request on a connection and only as a well-formed negotiate. Anything else disconnects. macOS opens with this packet. | `server/negotiate_wire_test.go` |
| `server/server.go` | SPNEGO and NTLM state is per connection. Upstream shared one authenticator. Work without an authenticated session is rejected. Logoff and tree disconnect close their handles. A listener error stops the server. Shutdown waits for workers. | `server/auth_wire_test.go`, `cleanup_test.go`, `session_gate_test.go`, `capabilities_test.go` |
| `server/file_tree.go` | FLUSH, CLOSE, WRITE, truncate and xattr errors are returned as SMB statuses. Write-through is honoured. Resource-fork and AFP-info streams support ranged reads and writes. Handles are checked against their session and tree. Xattr values and the list of xattr names are not logged. | `server/durability_test.go`, `error_wire_test.go`, `xattr_*_test.go`, `lock_wire_test.go` |
| `server/log.go` | The default logger writes to the application logger. | `server/redaction_test.go` |
| `internal/erref/erref.go` | `go:generate` line removed with its generator. | none |

Added files: `server/cleanup.go`, `server/request_validation.go`, `server/status.go`, `server/xattr.go`, `server/xattr_missing_darwin.go`, `server/xattr_missing_linux.go`, `vfs/xattr.go`.

Deleted: `internal/msrpc`, `stats`.

### xorm and mpb

`internal/thirdparty/xorm/engine.go` installs `log.SlogLogger`, added in `log/slog.go`, so the engine never builds the stdout SQL logger and never logs SQL arguments. Tests: `logging_s3smb_test.go`, `log/slog_test.go`. mpb has no changes.

## How to compare with upstream

```sh
git clone https://github.com/juicedata/juicefs /tmp/up && git -C /tmp/up checkout 0b90c7db5a929ae6adc5faad948d108efd2c99f9
m=github.com/djosh34/s3-smb/internal
find /tmp/up -name '*.go' -exec sed -i \
  -e "s|\"github.com/juicedata/juicefs|\"$m/juicefs|" \
  -e "s|\"github.com/macos-fuse-t/go-smb2|\"$m/smb-old/smb2|" \
  -e "s|\"xorm.io/xorm|\"$m/thirdparty/xorm|" \
  -e "s|\"github.com/vbauerster/mpb/v7|\"$m/thirdparty/mpb|" \
  -e "s|\"github.com/urfave/cli/v2|\"$m/thirdparty/cli|" \
  -e "s|\"github.com/hashicorp/golang-lru/v2|\"$m/thirdparty/lru|" {} +
diff -ru /tmp/up/pkg internal/juicefs/pkg | grep -v '^Only in /tmp/up'
```

The same steps work for the other three trees with their own repository and version.

## Rules

- No `replace` directive in `go.mod`.
- No `go.mod`, `go.work` or `vendor` under `internal/`. A nested `go.mod` drops that directory from the published module.
- No import of the original upstream paths. Such an import compiles and bypasses the patches.

`packaging_test.go` checks all three. `scripts/check-public-install.sh vX.Y.Z` installs a published version from empty caches.

## Licences

s3-smb's own code is AGPL-3.0-only. Each vendored file keeps its upstream licence and copyright notice: Apache-2.0 for JuiceFS, AGPL-3.0 for the SMB server, BSD-3-Clause for xorm, the Unlicense for mpb. Keep `NOTICE`, the four licence files and `internal/smb-old/smb2/Attributions.txt` in any copy. JuiceFS v1.4.1 ships no `NOTICE` file. If you distribute a modified version, publish the source of that version.
