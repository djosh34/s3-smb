# Code quality audit: s3-smb (HEAD ac712a6, 2026-10-03)

I ran everything in a detached worktree at /tmp/s3smb-audit. Tools: go1.26.3 linux/arm64, staticcheck, errcheck, deadcode, golangci-lint v2 (default linters, plus a second pass with funlen, gocyclo and dupl). I also cloned upstream JuiceFS and go-smb2 at the pinned commits and diffed them against the vendored copies.

## 1. Package map

| Dir | Purpose | Non-test LOC | Test LOC | Origin |
|---|---|---|---|---|
| main.go | Calls `app.Main` and works out the build version | 22 | 61 (packaging_test.go) | ours |
| internal/app | CLI, /dev/tty prompt, state flock, `serve()` startup/shutdown orchestration | 700 | 301 | ours |
| internal/backup | Scheduled SQLite metadata dumps to S3, delete protection window, recovery | 690 | 575 | ours |
| internal/config | YAML load/validate, secret sources, TLS, sizes | 621 | 811 | ours |
| internal/logging | slog setup, redaction, logrus/native bridges | 255 | 194 | ours |
| internal/smbfs | Adapter that implements the smb2 `vfs.VFSFileSystem` on JuiceFS `fs.FileSystem`: handles, path checks, byte-range locks, attrs/xattrs | 1,111 | 869 | ours |
| internal/storage | S3 client, volume identity/marker, encryption key, JuiceFS chunk-store/VFS setup | 445 | 1,198 | ours |
| internal/juicefs | JuiceFS v1.4.1 pkg subset (meta 17.8k, vfs 5.5k, chunk 4.3k, object 3.8k, utils/fs 3.3k) | 35,417 | 1,045 | vendored, Apache-2.0 |
| internal/smb2 | macos-fuse-t/go-smb2 SMB2/3 server (internal 12.0k, server 7.7k, vfs 0.6k) | 20,335 | 3,440 | vendored fork, AGPL |
| internal/thirdparty/xorm | ORM that JuiceFS SQL meta uses | 19,703 | 117 | vendored, BSD-3 |
| internal/thirdparty/mpb | Progress bars | 3,218 | 0 | vendored, unmodified |
| test/e2e | Runs the built binary against MinIO over SMB | 0 | 2,136 | ours |

- **Our code:** about 3,840 non-test LOC and about 6,090 test LOC. That is 1.6 test lines per code line.
- **Vendored code:** about 78,700 non-test LOC. Of the roughly 82.5k non-test LOC in the repo, 95% is vendored.

**How much JuiceFS is embedded, and how much it is changed:**
- The vendored copy keeps 35.4k of upstream's 72.2k non-test LOC in `pkg/` (49%). Only SQLite meta and the S3/file/mem object stores remain. `pkg/sync` and `fs/http.go` were removed.
- The diff against upstream 0b90c7d touches 16 files with +299/−230 lines. It also adds 3 new files totalling 185 LOC: `meta/protection.go`, `object/s3_smb.go` and `object/publication_smb.go`.
- The changes:
  - a maintenance or delete hook (`Config.CheckMaintenance`, `maintenanceTxn`)
  - session waitgroup on shutdown
  - `_synchronous=FULL`
  - `lockTxn` so locks work on read-only mounts
  - `DumpMeta` hardening
  - `BackupTo`/`PutIfAbsent`
  - `NoSuchKey` mapped to `ErrNotExist`
  - logger rerouting
  - `pread` coherence fix in `fs.go`

  This is a light, well-documented patch set. 24 files carry the "Modified for s3-smb" marker, and docs/vendored.md lists each change with the test that covers it.

**How much go-smb2 is changed:**
- 7 files are modified, +387/−203 lines. Most of it is in `conn.go` (+100/−86), `file_tree.go` (+134/−82) and `server.go` (+95/−19).
- 7 new files add 366 LOC: cleanup, request_validation, status, xattr*.
- 35 test files now exist under smb2, against 10 upstream. Most of the new ones are ours: wire, auth, malformed-packet and lock tests.
- The fork is more security-hardened than upstream: per-connection NTLM, mandatory signing, and validation of compound requests and bounds.

**xorm and mpb:** xorm is patched only to use a slog logger. mpb has no changes.

## 2. go.mod

- `go 1.26.3`, no `toolchain` line, no `replace`. `packaging_test.go` enforces no replace, no nested go.mod and no upstream imports.
- 39 direct and 36 indirect requires.
- Main deps: aws-sdk-go-v2 (s3, config, credentials), mattn/go-sqlite3 (cgo), sirupsen/logrus, prometheus/client_golang, emmansun/gmsm (SM2/SM4 crypto from JuiceFS), DataDog/zstd (cgo), hungys/go-lz4, google/uuid, yaml.v3, golang.org/x/{crypto,sys,sync,exp}, protobuf.
- Several deps are pulled in only by vendored code and are effectively dead weight:
  - redis/go-redis/v9 (`meta/utils.go`)
  - pkg/sftp (`object/file_unix.go`)
  - syndtr/goleveldb (`xorm/caches/leveldb.go`)
  - groupcache, gspt, dnscache, juju/ratelimit, VividCortex/ewma, pkg/errors

  All of these are linked into the binary.
- hirochachacha/go-smb2 is a test-only SMB client.

## 3. Error handling

`go vet ./...` is clean.

**golangci-lint (default linters): 729 issues.**

| Linter | Count |
|---|---|
| errcheck | 312 |
| staticcheck | 264 |
| unused | 77 |
| govet | 66 |
| ineffassign | 10 |

By area: thirdparty 423, smb2 154, juicefs 78, test/ 42, our internal packages 32. Only 12 of those 32 are in non-test code.

**Our non-test code:** 12 errcheck hits and 0 staticcheck findings. The single staticcheck hit in our tree is in a test: SA1019 `ReverseProxy.Director`, in `storage/transport_integration_test.go:311`. The 12 errcheck hits are all benign:
- `f.Close()` on error paths in `app/lock.go:28,32,37`
- deferred `os.Remove`/`f.Close` after an explicit checked close (`backup/backup.go:217-218`, `recovery.go:66,168,173`)
- read-only file closes in `config/config.go:110` and `secret.go:162`
- `fmt.Fprintln` in `app/main.go:35`

There is 1 `_, _ =` (`cli.go:85`, help printing) and 1 `_ =` (`r.listener.Close()` in `serve.go`). Our code has 0 `panic(`, 0 `recover()` and 0 `log.Fatal`. `os.Exit` appears only in `app` (`exitFailure`/`hardExit` watchdog). This is good discipline: write paths use `errors.Join(f.Sync(), f.Close())`.

**Vendored non-test code:** 208 errcheck hits (xorm 172 total, smb2 42, juicefs 27) and 49 `_ =` in juicefs. Other counts:

| Pattern | juicefs | smb2 | thirdparty |
|---|---|---|---|
| `panic(` | 29 | 9 | 11 |
| `recover()` | 2 | 0 | 1 |
| `logger.Fatal*` | 12 | 0 | 0 |
| `os.Exit` | 3 | 0 | 0 |

**Worst hotspots (all vendored):**
- `juicefs/pkg/meta/base.go:919-927`: the refresh goroutine calls `os.Exit(UmountCode)` if `format` reloads as unformatted, if its version is too new, or if its UUID changed. This bypasses `serve()`'s ordered shutdown, the state-lock logic and the SMB drain.
- `meta/interface.go:635-655` and `chunk/cached_store.go:831`: `logger.Fatalf` on bad URI or compression. These are startup-only and unreachable with our fixed config.
- `meta/sql.go:5265,5272`: `Fatalf` in `LoadMeta`, which recovery reaches.
- `meta/dump.go` (8×) and `sql.go:4702,4796`: `panic(err)` on write errors. `DumpMeta`'s `recover()` at `sql.go:4909` catches them; this pattern was patched.
- `meta/base.go:2868`: panic on invalid compaction.
- `smb2/vfs/attributes.go`: 5 panics on missing "mandatory" attributes. Our adapter must always set them.

## 4. Lint config and CI

- There is no `.golangci.yml`, no Makefile, and no staticcheck or gofmt step.
- Our dirs are gofmt-clean. 32 vendored files are not.

**`.github/workflows/test.yml`**
- Runs on every push and PR, on ubuntu-24.04.
- Steps: `go vet ./...` then `go test ./...`.
- No `-race`, the default 10-minute timeout, and no `-short`. No test checks `testing.Short()`, so `-short` makes no difference.

**`integration.yml`**
- Runs on pushes to main, PRs and manual dispatch, on ubuntu-24.04-arm with a 45-minute limit.
- `scripts/test-linux.sh` builds a pinned golang:1.26.3 image, builds MinIO from a pinned source commit, and runs `test/run-linux.sh`. That script does `go mod tidy -diff`, builds the binary, then `go test -race -count=1 -timeout=30m ./...` with `GOMAXPROCS=2` and `S3_SMB_E2E_ENDPOINT` set.
- This is the only place the race detector and the MinIO tests run.

**`macos.yml`**
- Manual dispatch only, on macos-15-intel.
- Installs a released version from the Go proxy and runs `test/macos/run.sh`, which is Python-driven real Time Machine.
- Modes: acceptance (backup, then recover on a fresh Mac), discover, and 5 failure scenarios (server kill/restart/cold/midpoint, client abort, machine loss).
- Timeouts: 30 to 150 minutes per job.

## 5. Build and test results

| Command | Result | Time |
|---|---|---|
| `go build ./...` | pass | 34 s cold |
| `go vet ./...` | clean | – |
| `go test ./... -short -count=1` | pass, all 25 packages with tests | about 7 s wall |
| `go test -race -count=1 ./...` (no MinIO) | pass | 57 s |

- The slowest packages are backup (5.0 s) and smbfs (4.8 s).
- Counts: 414 RUN, 368 PASS, 47 SKIP. Every skip says "needs MinIO". They are all of test/e2e (`TestRecovery`, `TestStartupRejectsBrokenRecovery/*`, `TestCrashDuringChunkPut`, `TestSMBToS3Smoke` and others), `storage/{bootstrap,transport}_integration_test.go` and `smb2/server/lock_minio_integration_test.go`.
- These are gated on `S3_SMB_E2E_ENDPOINT` and `S3_SMB_E2E_BINARY`, which means Docker plus MinIO through `scripts/test-linux.sh`. I did not run that script; it builds MinIO from source.
- Real Time Machine needs a Mac, through macos.yml.
- Several tests re-exec the test binary as a child, using `S3_SMB_*_CHILD` env vars. These run locally.

## 6. Overengineering and slop

**Dead code:**
- deadcode and U1000 find **0** dead functions in our code.
- Vendored code has 781 unreachable functions: smb2/internal/smb2 329, juicefs vfs 87, object 72, utils 55, mpb 66, xorm 79+. `unused` adds 77 items, 38 of them in smb2/server.
- Concrete removable weight:
  - **mpb (3.2k LOC)** is imported only by `juicefs/pkg/utils/progress.go`, and that function was patched to never draw. It is a pure no-op dependency.
  - **xorm dialects** for mysql, postgres, mssql and oracle total 3.5k LOC, though only SQLite is used. xorm/caches pulls in goleveldb.
  - `juicefs/pkg/vfs/internal.go`: the control-file message handler, `handleInternalMsg` (cyclomatic 62), is unreachable.
  - The redis and sftp imports exist only for error and type references.
- The vendored trees could plausibly shrink by about 10 to 15k LOC. This is a judgement call, because every deletion makes upstream diffs larger.

**Big functions (go/ast line counts, non-test):**

| Area | Function | Lines | Cyclomatic |
|---|---|---|---|
| ours | `app/serve.go:98 serve` | **264** | **79** |
| ours | `backup/recovery.go:151 Recover` | 133 | 33 |
| ours | `config/config.go:190 validate` | 72 | 47 |
| smb2 | `server/file_tree.go:41 create` | 298 | 70 |
| smb2 | `queryInfoFile` | 212 | – |
| smb2 | `sessionServerSetupChallenge` | 153 | – |
| juicefs | `meta/sql.go doRename` | – | 142 |
| juicefs | `doBatchUnlink` | – | 98 |
| xorm | `slice2Bean` | – | 122 |

- Other functions in our code are at most 71 lines. The average across our 141 functions is 22 lines.
- Largest files: juicefs `meta/sql.go` (6,177), `base.go` (4,118), smb2 `erref/ntstatus.go` (3,598), smb2 `server/file_tree.go` (2,627).

**Duplication:** dupl (threshold 150) finds 11 blocks, none of them in our code.

**Style:**
- Our code is not padded with comments, and the comments it has explain *why* (6% comment lines).
- The opposite problem: it is very dense. Only about 4% of lines are blank, and most functions have no blank line between them.
- 31 non-test lines are over 160 chars. The longest is 430 chars: one-line struct literals in `storage/runtime.go CacheConfig` and in `serve.go` `backup.New(...)`.
- Other non-idiomatic spots: `(error, bool)` returns (`smbfs/locks.go tryLocks`) and the ServerConfig field `Xatrrs` (typo inherited from upstream).
- There are no needless interfaces or layers. The four layers (app, storage, smbfs, smb2/juicefs) map onto real boundaries.

## 7. Assessment

**What "simple, clean, well-written Go" means here:** keep the approach of a small own core over thin patches to frozen upstream trees. That part is already good: 3.8k LOC, 0 dead code, 0 panics, errors wrapped with `%w`, ordered shutdown, and tests that outnumber code. Improvements:

1. **Split `serve()`** into named phases, all returning the same `resources`:
   - `openRemote` (list, identity, discover)
   - `decideVolume` (fresh, recover or existing; this could be a pure function over a struct of facts, which makes it table-testable)
   - `openMetadata`
   - `startBackups`
   - `startSMB`

   Its 79-branch decision tree is the most safety-critical logic in the repo and is only exercised end-to-end, behind MinIO.
2. **Format for readers:** use multi-line struct literals and put blank lines between functions.
3. **Add a checked-in `.golangci.yml`** scoped to our packages: errcheck, staticcheck, unused, gocyclo ≤ 30 and gofmt, with the vendored paths excluded. Add `-race` to the plain unit job, which costs about 57 s.
4. **Trim vendored code nobody calls:** mpb, the non-SQLite xorm dialects, and the JuiceFS control-file handler. Turn JuiceFS's `os.Exit` and `Fatalf` paths into errors that `serve()` handles.

**Biggest structural problem:** the per-open state is split three ways, which is what #124 describes and which also causes #123. The same open file exists as:
1. `smb2/server.Open`: a 30+ field struct, plus three `Server` maps (`opens`, `opensByGuid`, `deletePending map[uint64]bool`), all under one `Server.lock`.
2. `smbfs.handle`: path, `*jfs.File`, its own lock list, and a global `FS.mu`. Namespace operations take this as a write lock, at 19 call sites. That blocks all reads and writes while JuiceFS/SQLite runs.
3. JuiceFS: one shared writer per inode, plus plock rows in SQLite.

Path renames (`smbfs.Rename` updates descendants, `fileTree.setRename` updates only one `Open`), delete-pending identity, lock ownership (SMB reservation table plus POSIX ranges with rollback) and flush coherence are each decided in more than one place. `file_tree.go` (2,627 LOC, `create` at 298 lines and cyclomatic 70) decides separately in each handler whether an open is the base file or a stream.

Fix: make `smbfs` (our code) the single owner of open identity, meaning path, inode, stream, delete-pending and locks, behind a narrow interface. `smb2/server` should keep only protocol state (fileId, session, tree, oplock/lease) and call it. Write down the invariants (handle 0, base vs stream, who flushes) and test them through the real adapter, as the reviewers asked. Replace the global `FS.mu` write lock with per-path or per-inode locking once ownership is in one place. This is a focused refactor of about 1.1k LOC of adapter and the stream/delete paths in `file_tree.go`, not a rewrite of the SMB server.
