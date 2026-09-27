# Storage and durability plan audit

## Scope and result

Reviewed revision `fa36915f6a0de6fdb12b1292cc2102a79349071d`. The workspace HEAD matched it and `git status --short` was empty. This is a plan audit, not an application-code review.

Actual session environment, read from the process:

- `PI_MODEL=gpt-6-astra`
- `PI_PROVIDER=openai-codex`
- `PI_REASONING_LEVEL=xhigh`

Read the entire plan, README, CONTEXT, all issue bodies in `/tmp/s3-smb-plan-audit/issues.json`, the four supplied research notes, and relevant pinned source. I ran no builds or tests. Results attributed to research below are previously recorded tests, not tests executed in this audit. No source, caches, GitHub issues or commits were changed.

The plan correctly requires consistent SQLite exports, disabled writeback, fail-stop backup protection, one writable metadata authority and real SMB/S3 acceptance tests. It correctly limits the earlier local-object probes. Seven material corrections follow. None reopens those decisions or the approved `AGPL-3.0-only` license.

## Findings

### 1. High: native file flush does not establish the promised local metadata synchronization

**Evidence.** Plan line 126 promises that successful FLUSH/write-through completes remote upload and local metadata synchronization. Issue #22 repeats this. JuiceFS file `Flush` and `Fsync` only call the data writer's `Flush`, at [J1] lines 1440 to 1465. JuiceFS selects WAL without specifying synchronous mode, at [J2] lines 448 to 457. The pinned SQLite driver defaults to `NORMAL` and explicitly selects it for WAL, at [D1] lines 1107 and 1314 to 1319. SQLite's own source says ordinary WAL commits under NORMAL do not sync, at [D2] lines 60689 to 60705.

**Failure scenario.** An SMB flush can upload the data and commit its mapping, return success, then lose the mapping after a host power failure. A process-kill/restart test can pass because the kernel's dirty pages survive that test. Accepted loss since the last remote metadata backup does not excuse violating the separate local flush promise.

**Smallest correction.** Specify `_synchronous=FULL` on every SQLite connection, or an explicitly verified equivalent durability operation. FULL is the smaller change and does not require remote metadata on every write. Add a regression for the effective pragma on pooled connections and a storage fault test that checks synchronization errors reach SMB. Do not describe SIGKILL alone as a power-loss test.

**User decision needed.** No. This implements the existing FLUSH contract. This finding is source inspection; no power-loss test ran here.

### 2. High: native backup success can conceal a truncated gzip export

**Evidence.** Plan lines 132 to 134 and issue #24 reuse native JSON/gzip backup and require staging failures to preserve the prior point. The native helper assigns the export error, then executes `_ = zw.Close()` and proceeds to upload if the export itself succeeded, at [J3] lines 124 to 140.

**Failure scenario.** The final compressed bytes or gzip footer hit ENOSPC. `DumpMeta` already returned nil, but gzip finalization fails. The helper can upload the truncated file and report success. The application then accepts a bad recovery point and eventually allows deletion of data needed by the previous one. An export-error test or an unwritable-directory test does not exercise this boundary.

**Smallest correction.** Include gzip finalization and staging I/O errors in the result-returning backup operation. Do not upload, advance protection or rotate backups after any such error. Add an injected writer failure specifically during gzip close, followed by a cold restore of the unchanged prior point. Record the narrow native correction in the source manifest.

**User decision needed.** No. The ignored return value is demonstrated source behavior; the ENOSPC sequence is a source-derived failure scenario, not a newly executed probe.

### 3. High: the plan does not protect native trash from SMB purges

**Evidence.** Plan lines 117 to 124 require ordinary namespace operations and handle identity; line 138 keeps native trash. Neither those sections nor issue #22 defines the native identity for the single SMB account or protects the trash namespace from client purges. Native lookup resolves `.trash`, at [J4] lines 1273 to 1278. `Unlink` permits deletion inside trash for UID 0, at lines 1781 to 1792. `toTrash` returns false for an already-trashed parent, at lines 3012 to 3016. `meta.Background()` and `WrapContext` use UID 0, at [J5] lines 39 to 41 and 92 to 94.

**Failure scenario.** If the adapter uses the convenient root context, an authenticated SMB client can delete a file and then purge its native trash entry before the next backup. That bypasses the retention assumption even while the last snapshot is fresh. This is a conditional integration hazard, not a claim that the nonexistent adapter already does so.

**Smallest correction.** Define the account's native context and reserve native trash from SMB mutation. Reject purge or move-out operations by resolved identity, including aliases, rather than merely hiding `.trash` in directory listings. Keep ordinary file deletion through native trash. Add a real SMB regression that deletes a file, attempts to purge its retained entry, and cold-restores the protected snapshot.

**User decision needed.** No. This protects the agreed recovery point; it does not require a trash-management product.

### 4. High: the cleanup-age rule needs an enforcement point inside native deletion work

**Evidence.** Plan lines 95, 109 to 113 and 138 require startup and ongoing protection gates. Issue #24 asks for clock-controlled tests but does not identify how the native cleanup code consults that gate. `NewSession` starts deletion workers outside the `NoBGJob` condition, at [J4] lines 800 to 812. Reads can trigger compaction at lines 2120 to 2128. Native trash maintenance takes its own retention cutoff, at lines 3062 to 3117 and 3249 to 3264. SQL delayed cleanup retires references and queues deletions, at [J2] lines 3803 to 3861, without any application backup-age input.

**Failure scenario.** After a long process suspension, native cleanup can resume before the application's overdue-backup callback. Checking age only when starting maintenance or scheduling backups cannot prevent this. Canceling maintenance after the callback also does not establish what happens to work already queued. The recorded writable-recovery probe used explicit cutoffs; it did not test this race.

**Smallest correction.** Specify the narrow native integration that checks protection before destructive retirement/deletion, including queued work. Close that gate immediately on protection failure, before draining SMB requests. Keep native retention algorithms, but do not treat `NoBGJob` as the gate. Test the real callbacks with a controlled clock: pause the backup scheduler, advance beyond the safe age, release cleanup first, and prove no protected object is deleted and the prior snapshot still restores. Define the timing inequality against the effective volume retention, not only the YAML value.

**User decision needed.** No. The required guarantee is already agreed; its execution mechanism is missing.

### 5. High: the resource-fork write path silently acknowledges xattr failures

**Evidence.** Plan line 117 prohibits silent mutation success; lines 121 to 126 cover macOS attributes and FLUSH corrections. Issue #23 names FLUSH, CLOSE and write-through error fixes. The pinned SMB `writeImpl` has `// ignore xattr errors`, calls `Setxattr` without assigning its error, then sets the reported byte count to the request length, at [S1] lines 829 to 835.

**Failure scenario.** A resource-fork or other extended-attribute stream write gets EROFS, ENOSPC or another backend error, but SMB reports successful WRITE. Correcting the ordinary data-write and FLUSH paths does not fix this. Successful binary-xattr round trips do not expose it either.

**Smallest correction.** Add this demonstrated error path to issue #23's patch scope. Propagate `Setxattr` failure to the protocol response. Add a wire-level regression with an injected xattr failure and a read-only stream-write attempt, and verify that no bytes are acknowledged as stored.

**User decision needed.** No. The discarded error is source-verified; this audit did not run an SMB reproduction.

### 6. Medium: bounded shutdown is not supplied by the selected native close methods

**Evidence.** Plan lines 99 and 111 require bounded resource closure; issue #21 also requires native tasks to stop on close. Native file `Close` invokes the writer with `meta.Background()`, at [J1] lines 1468 to 1490. Writer flush has an internal timeout of at least five minutes, at [J6] lines 386 to 429. Filesystem `Close` closes the metadata session, not the SQL engine, at [J1] lines 1093 to 1110; SQL engine shutdown is separate at [J2] lines 547 to 550. The writer and filesystem constructors start loops with no stop condition, at [J6] lines 470 to 512 and [J1] lines 246 to 280. The cache constructor also starts a perpetual loop, at [J7] lines 874 to 884.

**Failure scenario.** An S3 stall during handle close can outlive the proposed 30-second shutdown. A wrapper that returns on a timer may leave native workers using resources that it then closes. Even a clean close does not satisfy an unconditional in-process no-goroutine-leaks assertion with these native loops.

**Smallest correction.** Add an explicit lifetime map to issues #21/#25: stop destructive work, drain requests, flush and close tracked handles, stop/join mutable workers, close the session, call metadata shutdown, then release the state lock. Name the required narrow cancellation/join changes. The foreground process must exit nonzero at the hard deadline rather than release the lock while a writer remains alive. Permit harmless process-lifetime native loops to end at process exit if that is the intended scope, instead of requiring an unnecessary full rewrite. Test shutdown against stalled real native I/O in a subprocess.

**User decision needed.** No. This is an implementation detail and a correction to an overbroad lifecycle assertion, not a new operating mode.

### 7. Medium: second-resolution backup names can overwrite the prior recovery object

**Evidence.** Plan line 109 requires a backup on every writable startup. Lines 113 and 132 require preservation of the prior point on failure. Native names contain only whole seconds, at [J3] line 102, and upload directly to that key at lines 134 to 140. Native rotation assumes the fixed filename shape, at lines 200 to 207. No collision rule appears in issue #24.

**Failure scenario.** Two quick startups, a short configured interval or a backward clock adjustment can reuse a key. An upload accepted by S3 whose response is lost can replace the prior object even though the operation reports failure. Without object versioning, the prior bytes are gone. No concurrent writer is needed. This is a naming and failure-semantics inference, not a tested provider failure.

**Smallest correction.** Never target an existing backup key. With the existing single-writer assumption, check for a collision and wait or fail safely rather than overwrite. Preserve the actual snapshot timestamp for age accounting. Alternatively, use unique names and make the matching narrow retention-parser change. Add equal-timestamp, backward-clock and accepted-upload/lost-response tests; assert the previous object's bytes remain unchanged.

**User decision needed.** No. Do not impose bucket versioning as a new requirement to solve this.

## Evidence boundaries

The prior research demonstrated mixed states in slow SQLite export and expiry of blocks referenced by old dumps. The plan addresses those findings. The later writable-recovery probe demonstrated safe reuse of unreferenced later object IDs in its tested sequence. I found no basis to reopen that decision, redesign allocation counters, require another dataset or make recovery permanently read-only.

The proposed tests remain acceptance work. They are not proof of S3 failure behavior, power-loss durability or Time Machine compatibility today. In particular, tests must assert recoverability and actual deletion behavior, not only error returns, task counts or backup-object existence.

## Passphrase-only encryption-secret recovery

Yes, this is technically feasible without deriving an RSA key from a password. The daemon can generate a random native-compatible key and retain a standard passphrase-encrypted copy in S3. The user then needs only the passphrase as the separately retained encryption secret. The key still exists, but the user need not separately preserve its file.

This is a source-supported design option, not an implemented or tested recovery path. JuiceFS accepts encrypted PKCS#8 through `ParsePrivateKeyFromPem`, at [K1] lines 67 to 112. Its existing dependency exposes encrypted PKCS#8 encoding and configurable password-based encryption, at [K2] lines 19 to 24 and 91 to 125. Avoid choosing JuiceFS's legacy `x509.EncryptPEMBlock` exporter merely because it is named native, at [K1] lines 47 to 60. Select reviewed PKCS#8 encryption and password-derivation parameters through the maintained library.

If approved, the smallest design needs:

- A documented per-volume S3 bootstrap location, outside the object-encryption wrapper that requires the same key and outside metadata-backup rotation.
- Successful upload, download and native-parser verification of the protected key before writable serving. Interrupted initialization must not replace an existing key.
- A recovery test that removes every local key/config/state/cache file and restores using only the passphrase, S3 access and documented nonsecret connection/volume information. Missing, corrupted or wrong-passphrase key objects must fail without replacement.

Anyone who obtains that protected object can attempt offline password guesses. A strong passphrase matters. Losing both the remote protected key and all optional independent copies remains unrecoverable. None of this removes the need for separate S3 credentials, or lets the passphrase reconstruct a missing random key.

Plan lines 86 to 88 explicitly label the old external-PEM workflow unapproved. Keep it that way. If this option is selected, replace that workflow and update configuration and recovery tests; do not preserve mandatory external PEM retention by accident.

## Source locations

All plan line references identify `/home/joshazimullah.linux/work_mounts/s3-time-machine/docs/implementation-plan.md` at the reviewed revision. Issue references identify the corresponding numbered body in `/tmp/s3-smb-plan-audit/issues.json`.

Pinned JuiceFS source is `0b90c7db5a929ae6adc5faad948d108efd2c99f9`, in the import-relocated research copy. SMB is `277a9300411249a881a05f7a910f5a83ae3395f2`.

- [J1] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/fs/fs.go`
- [J2] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/meta/sql.go`
- [J3] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/vfs/backup.go`
- [J4] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/meta/base.go`
- [J5] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/meta/context.go`
- [J6] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/vfs/writer.go`
- [J7] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/chunk/cached_store.go`
- [D1] `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/mattn/go-sqlite3@v1.14.24/sqlite3.go`
- [D2] `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/mattn/go-sqlite3@v1.14.24/sqlite3-binding.c`
- [S1] `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/macos-fuse-t/go-smb2@v0.0.0-20260921092600-277a93004112/server/file_tree.go`
- [K1] `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/object/encrypt.go`
- [K2] `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/emmansun/gmsm@v0.41.1/pkcs8/pkcs8.go`

Research notes read in full:

- `/tmp/s3-time-machine-bundled-research/docs/research/bundled-source-install.md`
- `/tmp/s3-time-machine-recovery-research/docs/research/metadata-backup-and-recovery.md`
- `/tmp/s3-time-machine-writable-research/docs/research/writable-recovery.md`
- `/tmp/s3-time-machine-logging-research/docs/research/embedded-logging.md`

## Genuine user questions and recommended answers

1. Must S3 credentials refresh without restarting? This concerns the access key, secret key and optional session token used to authorize S3 requests. TLS certificates instead establish HTTPS trust and, for mutual TLS, client identity. Certificate replacement does not renew an expiring S3 session token. Recommend startup-only resolution for the first release if restarting after credential changes is acceptable. If you use expiring credentials unattended, choose refresh now and specify a coherent access-key/secret/token refresh policy before implementation.
2. May the daemon store a strongly passphrase-protected native key in S3 so that you retain only the passphrase as the encryption secret? Recommend this option, subject to the bootstrap and recovery tests above. S3 access and nonsecret location information remain separately necessary. This remains your choice, not an approval inferred from your question.

After those answers and the material corrections, ask for final backlog approval under issue #29. Do not ask again about AGPL-3.0-only, writable recovery, hourly backup or the configurable 14-day trash default.
