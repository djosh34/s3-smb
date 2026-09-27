# Handoff: macOS Time Machine to S3 through a local SMB server

Historical research input, updated with the confirmed first-release boundary. No application has been built or tested. Current decisions live in the [GitHub planning map](https://github.com/djosh34/s3-time-machine/issues/1).

**Current scope:** a terminal-only daemon installed with `go install`, with SQLite as its only CGo dependency. Build and test the SMB → embedded JuiceFS → S3 machinery on Linux/ARM64 first. The user will test macOS later; actual Time Machine integration is deferred. Reuse JuiceFS's in-process cache, with no FUSE dependency. Explicitly support remote data exceeding available local storage. Time Machine-specific procedures below are historical proposals, not first-release acceptance criteria.

## Start here

Build a terminal-operated SMB-to-S3 daemon. The eventual motivating client is Time Machine on the same Mac, but first validate the storage machinery independently on Linux. Store the backup data in S3-compatible object storage through embedded JuiceFS, using its officially supported SQLite metadata backend. Avoid a separate server and a FUSE mount. The user accepts losing an unfinished backup but wants previously completed recovery points to survive loss of the Mac and its local database/cache.

The user explicitly accepts CGo for the official SQLite backend. The SMB direction is a focused go-smb2 fork with a direct JuiceFS adapter. Source inspection found an ignored flush error and optional no-op locking that must be addressed. AGPLv3 is the planned project license. SlateDB is no longer the preferred initial approach. Outstanding work includes remote metadata backups, data retention, recovery on another Mac, and validation of the local SMB integration.

The proposed safety policy is 14-day JuiceFS trash retention, hourly metadata exports, and an additional consistent SQLite backup after each completed Time Machine run. Those numbers and the completion routine were assistant recommendations, not separately confirmed user decisions.

## 1. Decisions and constraints

| Topic | Status | Decision or context |
| --- | --- | --- |
| Workload | First-release scope | SMB filesystem operations backed by S3. Native Time Machine integration comes later. |
| Storage | User requirement | S3-compatible object storage, including services such as Backblaze B2. A provider has not been selected. |
| Local storage | Explicit requirement | The remote dataset may exceed the daemon's available local storage; use JuiceFS's own in-process caching. |
| Deployment | Confirmed first-release direction | Terminal-only daemon, installed with `go install`; Linux/ARM64 testing first, macOS testing later. |
| Interface | Confirmed direction | Expose SMB through a direct in-process filesystem adapter. |
| FUSE | Explicit exclusion | No FUSE dependency or mount. |
| Filesystem engine | Explicit user choice | JuiceFS. |
| Metadata engine | Latest explicit user choice | Official JuiceFS SQLite backend. |
| Language | Confirmed direction | Go; SQLite is the only permitted CGo dependency, including the transitive build graph. |
| SMB integration | Current project direction | Focused go-smb2 fork and direct in-process JuiceFS adapter. Preserve the protocol implementation and patch correctness issues. |
| Project license | Planned choice | AGPLv3 for our combined application. The user proposed this and requested clarification; see licensing below. No repository license file has been created yet. |
| Development | Explicit user willingness | Writing the app and integration code ourselves is acceptable. |
| Reliability | Primary user requirement | A crash during today's backup must not make yesterday's protected recovery point unusable. |
| Acceptable loss | Explicit user statement | Losing the latest unfinished backup is acceptable. |
| Encryption | User requirement | Do not rely solely on the storage provider's encryption. Client-controlled encryption and recoverable keys are needed. Exact scheme remains undecided. |
| Experience | User priority | Fast, reliable, and responsive backup browsing. Cache metadata and frequently read blocks; retrieve old contents on demand. |
| Alternatives | Explicit exclusion | Do not recommend restic. The user has tried it and does not want it. |

## 2. Proposed architecture

Time Machine connects to an SMB server in the local app. A filesystem adapter connects the SMB server to JuiceFS's in-process filesystem API. JuiceFS uses local SQLite metadata and uploads file data to S3-compatible storage. An app-controlled process creates encrypted remote metadata recovery points.

The local disk holds the active SQLite database, its journal/WAL, a bounded data cache, and temporary backup files. The bucket holds JuiceFS data objects and remote metadata backups. Recovery must also have access to credentials, configuration and encryption keys outside the lost Mac.

### Candidate implementation pieces

- SMB library: use a focused fork of `macos-fuse-t/go-smb2` as the current direction, subject to integration tests. Import its `server` and `vfs` packages and provide our own backend and app lifecycle. Its README says it targets macOS/Time Machine and warns that some protocol features are incomplete or early. [S1]
- JuiceFS: investigate `pkg/fs.NewFileSystem` and the file APIs for offset reads/writes, sync and extended attributes. These were identified as the likely integration route; re-check the pinned version before implementing. [S2]
- SQLite: retain JuiceFS's supported SQL metadata implementation. Custom replication/checkpoint coordination may surround it without replacing the metadata backend.
- Background lifecycle: explicitly wire backup scheduling, uploads, cache management, shutdown and cleanup. Do not assume importing the library starts every background task normally started by the JuiceFS CLI.

No loopback SMB setup, Time Machine destination registration, privilege model, macOS version support or restore workflow has been demonstrated. A working SMB library is not proof of end-to-end compatibility.

### SMB source findings and required fixes

Inspected upstream commit `277a9300411249a881a05f7a910f5a83ae3395f2`. These are static source findings, not demonstrated end-to-end failures or completed fixes. [S15]

- No direct `import "C"`, CGo directives or FUSE dependency was found in this SMB source tree. `macos-fuse-t` is the owner name. Its `vfs` package defines a Go filesystem interface; it is not a FUSE mount. A full transitive dependency build with CGo disabled has not been performed.
- The example `smb2_server.go` wires `NewPassthroughFS` into `server.NewServer`. Replace that backend with our JuiceFS adapter. The sample CLI, local-directory implementation and configuration/logging setup need not be imported into our app. Deleting their source files is not required to exclude them.
- `server/file_tree.go:617-642`, `fileTree.flush`, calls `t.fs.Flush(...)`, discards its error, then sends success. Patch it to validate the request/handle appropriately and return the correct SMB error when the backend fails. Never acknowledge a failed storage flush as successful.
- `fs_impl.go:129-131`, the sample `PassthroughFS.Flush`, returns nil without syncing. A separate `FSync` calls `os.File.Sync`, but the inspected SMB flush handler does not call it. Our adapter must implement meaningful flush semantics; merely filling in `FSync` will not fix this path.
- `vfs/vfs.go:89-93` defines optional `ByteRangeLocker`. Its comment states that backends without it accept locks as a no-op. Implement the required locking behavior and test conflicts, unlocks and handle-close/disconnect cleanup.
- Audit SMB write-through flags as a distinct path. Constants exist, but source searches did not find corresponding handling in the server code. This is a review lead, not proof of every affected behavior.
- JuiceFS `pkg/fs/fs.go` exposes `Pread`, `Pwrite`, `Flush` and `Fsync`. In the inspected source, both file flush methods call the writer's `Flush`. Confirm the exact data/metadata durability contract of the pinned version and preserve error propagation through every layer. [S2]

Keep macOS-specific attributes, negotiation, authentication and locking machinery until compatibility tests establish what can safely be omitted. The purpose of the fork is a small integration and correction patch set. No fork or fixes have yet been created.

### Licensing direction

Use AGPLv3 for the combined app. Upstream offers AGPLv3 or a separately negotiated commercial license. Under the AGPL route, a distributed derivative app must meet its copyleft and corresponding-source requirements; labeling the whole fork MIT or Apache would not remove those obligations. Preserve upstream copyright/license notices and modification notices. [S1, S16]

Provide recipients with the corresponding source for released binaries, including relevant build material. For modified versions used by remote network users, section 13 requires a prominent source offer. Personal local development does not by itself require publishing a public repository. Choose a release/source-offer mechanism when packaging the app. [S16]

JuiceFS Community Edition is Apache-2.0. Preserve its license and any applicable notices in the combined distribution. The project-level AGPL choice does not erase dependency licenses. Check all included dependencies before release. [S17]

Recommendation for our own new code: `AGPL-3.0-only`, unless we deliberately choose to grant later-version permissions. Verify upstream's exact grants before using an `-or-later` label for the whole combined work. Buying an appropriate commercial upstream license or replacing the AGPL implementation are alternative routes; neither is currently planned.

## 3. Recovery model and the failure that matters

Two histories exist:

1. Time Machine's restore points inside its backup disk image.
2. Saved JuiceFS metadata states that can rewind the entire outer filesystem, including that image.

JuiceFS sees sparsebundle band files and their block mappings. It does not directly index every original document inside Time Machine's filesystem. That inner filesystem's indexes are stored in image data blocks, so a local SQLite database alone does not guarantee fast browsing.

### Why an old database backup can become unusable

Suppose yesterday's database maps a file to objects A and B. Today JuiceFS compacts them into C and updates the live mapping. After old-object retention expires, A and B may be deleted. Restoring yesterday's database then produces references to missing objects. This can happen even if compaction left the logical file contents unchanged.

Source inspection confirmed the mechanism in JuiceFS `pkg/meta/base.go` and `pkg/meta/sql.go`, notably `compactChunk`, `doCompactChunk`, `cleanupDelayedSlices`, and `doCleanupDelayedSlices`. Trash retains replaced slices as well as deleted files. Cleanup uses their retirement timestamps and the configured retention period. A saved metadata file does not permanently pin its referenced objects. [S3, S4]

The essential recovery requirement is a consistent saved metadata state plus every data object it references, plus the necessary keys and configuration.

### Meaning of checkpoint

- A SQLite WAL checkpoint folds logged changes into the active database. It does not create a separate historical recovery point. [S5]
- A SQLite backup made through its backup API is a consistent database copy. [S6]
- A JuiceFS metadata dump is a portable export. Its documentation warns that exports during concurrent changes do not generally provide snapshot consistency. Do not equate an arbitrary hourly dump with a verified completed Time Machine recovery point. [S7]
- A completed Time Machine recovery point is an application-level state. A database-consistent copy taken during a backup is not automatically evidence that the image was cleanly detached or the backup completed.

### Durability and deletion rules for the design

- Preserve JuiceFS's normal data-before-metadata upload ordering. Its normal flush uploads data blocks before updating metadata. Optional writeback mode allows flushes to complete after local caching, so it cannot be treated as remote durability. Initial recommendation: leave writeback off. [S8]
- Ordinary writes may be buffered. Correctly implement SMB flush and any write-through requests. Do not conflate successful local SQLite commit with remote recoverability.
- A protected older recovery point must survive creation or failure of a newer one. Publish a new point only after its data and consistent metadata backup are remote.
- Garbage collection and retention must preserve objects needed by the advertised recovery window. Bucket lifecycle settings must not delete live data merely because the object itself is old.
- Restore an old database in isolation with read-only access and background deletion disabled initially. Before allowing writes, resolve effects of stale sessions, counters/object IDs and cleanup against the shared bucket. This is an implementation review item, not a tested recovery procedure.

Basic crash recovery does not require detaching the image after every write. Clean detach was proposed specifically to create an easily understood completed-run recovery point.

## 4. Latest proposed SQLite safety policy

These are recommendations awaiting implementation and validation, not built-in guarantees.

| Setting or action | Proposed choice |
| --- | --- |
| JuiceFS trash retention | 14 days initially |
| Automatic JuiceFS exports | Hourly |
| Additional recovery point | After each successfully completed Time Machine run |
| Retain completed recovery points | At least the last 7 days, within the 14-day block-retention window |
| Upload failure | Report immediately and retain the previous good point |
| Last successful completed point reaches 7 days old | Suspend app operation and cleanup before the remaining safety margin is consumed |
| Startup after an absence | Check recovery-point age before starting cleanup jobs |

The suggested setting for an existing volume was:

```sh
juicefs config sqlite3:///absolute/path/metadata.db --trash-days 14
```

Seven days was considered workable if failures are noticed quickly. Fourteen days gives more recovery time. Measure actual retained-object churn before deciding whether the cost justifies shortening it. Changing retention cannot restore objects already deleted.

### Proposed completed-run procedure

1. Confirm Time Machine completed and cleanly detached its image.
2. Finish all relevant JuiceFS uploads and metadata updates. Prevent new application writes while establishing the recovery point.
3. Make a consistent SQLite backup through SQLite's supported backup API.
4. Compress, encrypt and upload it under a unique name.
5. Publish a small recovery record only after success. Record version/configuration, timestamp, object identity and checksum; exact format is open.
6. Retain the prior successful points and their required data for the promised period.

The exact way to detect completion and detach, exclude a new run, coordinate background compaction, and handle suspension or power loss mid-procedure is unresolved. A clean SQLite backup alone does not prove the whole procedure correct.

### Stock defaults versus app additions

JuiceFS normally exports metadata hourly to the bucket's `meta` prefix. The interval is configurable. Its documented export retention keeps all exports for two days, then daily, weekly and monthly samples as they age. That export retention does not ensure corresponding old data blocks survive equally long. Large namespaces can cause scheduled exports to be skipped under documented thresholds; observe success, not merely configuration. [S7]

Completed-run SQLite copies, success tracking and the cleanup-age guard would be our app's additions. The stock trash default is one day. [S9]

## 5. Native caching and remote storage

The S3 dataset may exceed the daemon's available local storage. Reuse JuiceFS's in-process cache and retrieve uncached contents on demand; do not introduce FUSE or a parallel cache implementation.

Verify cache configuration, eviction, disk-pressure behavior, and background lifecycle in the selected embedded JuiceFS version. Exercise a dataset larger than the configured cache and confirm evicted data remains readable from S3. Request latency, flush frequency, compaction, and cache misses need measurement rather than assumed throughput guarantees. [S8]

## 6. Encryption and recovery secrets

The user explicitly distrusts relying only on provider-side encryption. Earlier discussion raised JuiceFS client-side encryption and Time Machine's own encryption, but no final combination, algorithm or key-management design was chosen.

Before implementation, decide:

- Whether to use JuiceFS encryption, Time Machine encryption, or both, and what each protects.
- How to encrypt custom raw SQLite backup copies. They are outside the automatic JuiceFS-export path and must not be assumed encrypted by it.
- How to protect credentials and keys locally and recover them after losing the Mac.
- What filenames, filesystem structure and metadata remain visible to the provider.
- What the minimal recovery package contains and how a fresh Mac locates it.

JuiceFS's documentation distinguishes encrypted automatic metadata backups from plaintext manual `dump` output. Do not extrapolate that behavior to arbitrary raw SQLite copies. [S7]

## 7. Alternatives investigated and current disposition

- S3QL and other object-backed filesystem approaches were discussed earlier. The user selected JuiceFS. No further comparison is needed unless a concrete blocker appears.
- Direct custom SMB-to-S3 would require owning filesystem metadata, random writes, caching, ordering, retention and recovery. Current direction reuses JuiceFS.
- SlateDB is technically plausible as a custom JuiceFS transactional KV backend. JuiceFS already has `kvtxn` and `tkvClient` interfaces. SlateDB supports transactions and scans. The user subsequently preferred standard SQLite, so do not restart this debate by default. [S11]
- SlateDB would add remote commit latency, storage operations, a native Rust library through CGo, and a custom backend to maintain. Its database checkpoints protect its own database files, not JuiceFS's separate data objects. [S12]
- Litestream was explored for SQLite replication and explicit remote sync. It is not selected. Its asynchronous replication must not be confused with remote commit durability. The separate VFS write mode documents buffered periodic uploads and consistency limitations. [S13]
- A hosted metadata database or separate NAS is outside the chosen initial deployment direction.
- Restic is explicitly unwanted.

## 8. Historical next-work proposals

The current Linux-first daemon backlog supersedes this order. Actual Time Machine validation is deferred.

1. Later macOS phase: validate the local SMB route. Time Machine acceptance, backup completion, browsing and file restoration are separate integration evidence.
2. Pin SMB/JuiceFS versions and map filesystem operations. Fix the identified flush-error path, implement adapter flush/locking semantics, and test failed flushes and write-through handling. Completion means a concrete adapter plan and evidence that storage failures cannot be silently acknowledged.
3. Specify the SQLite recovery-point state machine and cleanup guard. Completion means each crash boundary has an explicit recoverable previous state and no cleanup can outrun it.
4. Resolve provider configuration, encryption, key recovery and retention without reopening settled architecture unnecessarily.
5. Build the smallest end-to-end prototype with a small bucket and dataset. Keep production backup data out of the experiment.
6. Test loss of all local state during a later backup and restore a prior completed point from bucket plus recovery secrets. Also test metadata-upload failures, app restart after retention-age thresholds, cache exhaustion, compaction and interrupted recovery-point publication.
7. Measure initial/incremental throughput, browse latency, restore speed, DB/WAL sizes and object churn. Use results to refine caching and retention.

The user has authorized discussion and this handoff, and expressed willingness to build. No credentials, bucket, repository, deployment or exact product scope have been supplied. No integration has been tested. The key acceptance test is recovering yesterday after deleting every local app state file during today's backup.

## 9. Working style and suggested skills

The user wants plain explanatory language first, followed by the correct technical term. Apply `unslop`: direct prose, no hype, no unnecessary em dashes or ornate phrasing. Check factual claims against primary sources and inspect source code when relevant. Distinguish confirmed behavior, inference and proposals. Do not claim Time Machine-on-JuiceFS is well proven or forum-endorsed; no dependable end-to-end evidence was established in this conversation.

Suggested skills when applicable:

- `unslop` for all user-facing writing.
- `codebase-design` for the SMB adapter and storage interfaces.
- `domain-modeling` to define recovery point, remote completion and retention consistently.
- `prototype` for the compatibility experiment.
- `to-spec` once open choices are resolved and a project tracker exists.
- `implement` when there is an agreed implementation scope.
- `diagnosing-bugs` for failures found in recovery/performance testing.
- `handoff` to update this document at a future boundary.

## 10. Sources and workspace state

Primary sources checked during the conversation, most recently 2026-09-27. Links to `main` are mutable; pin commits before implementation.

- **S1:** SMB library and its stated limitations: https://github.com/macos-fuse-t/go-smb2
- **S2:** JuiceFS in-process filesystem source: https://github.com/juicedata/juicefs/tree/main/pkg/fs
- **S3:** JuiceFS trash and replaced slices: https://juicefs.com/docs/community/security/trash/
- **S4:** Inspected implementation: https://github.com/juicedata/juicefs/blob/main/pkg/meta/base.go and https://github.com/juicedata/juicefs/blob/main/pkg/meta/sql.go
- **S5:** SQLite WAL/checkpoint meaning: https://sqlite.org/wal.html
- **S6:** SQLite backup API: https://sqlite.org/backup.html
- **S7:** JuiceFS exports, consistency warnings, schedule, retention and encryption distinctions: https://juicefs.com/docs/community/metadata_dump_load/
- **S8:** JuiceFS data flow, block layout and writeback semantics: https://juicefs.com/docs/community/internals/io_processing/
- **S9:** JuiceFS command options: https://juicefs.com/docs/community/command_reference/
- **S10:** SQL metadata sizing and SQLite setup: https://juicefs.com/docs/community/databases_for_metadata/
- **S11:** JuiceFS KV interface: https://github.com/juicedata/juicefs/blob/main/pkg/meta/tkv.go ; SlateDB Go API: https://pkg.go.dev/slatedb.io/slatedb-go/uniffi
- **S12:** SlateDB binding/dependencies: https://github.com/slatedb/slatedb/blob/main/bindings/go/README.md ; checkpoints: https://slatedb.io/docs/design/checkpoints/
- **S13:** Litestream sync: https://litestream.io/reference/sync/ ; VFS write limitations: https://litestream.io/guides/vfs-write-mode/
- **S14:** Related request to make JuiceFS SQLite metadata object-backed through a custom VFS: https://github.com/juicedata/juicefs/issues/7250 . Background only; not the selected route.
- **S15:** Inspected SMB commit: https://github.com/macos-fuse-t/go-smb2/tree/277a9300411249a881a05f7a910f5a83ae3395f2 ; flush handler: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/server/file_tree.go#L617-L642 ; sample backend: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/fs_impl.go#L115-L131 ; locking interface: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/vfs/vfs.go#L89-L93
- **S16:** Upstream AGPLv3 text, especially sections 2, 5, 6 and 13: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/LICENSE
- **S17:** JuiceFS license: https://github.com/juicedata/juicefs/blob/main/LICENSE ; Apache compatibility explanation: https://www.apache.org/licenses/GPL-compatibility.html

No project repository, tickets, commits or application implementation files were created. Temporary research downloads are in `/workspace/scratch/11fb5fb77d9a/`: the inspected SMB checkout is `go-smb2-277a9300411249a881a05f7a910f5a83ae3395f2/`; other research files include `juicefs-tkv.go`, `juicefs-sql.go`, `juicefs-base.go`, `juicefs-backup.go`, `juicefs-sql-sqlite.go`, `juicefs-fs.go`, `slatedb-go-readme.md`, and `slatedb-go-list.json`. JuiceFS/SlateDB downloads are unpinned snapshots. These are research inputs, not project code, and may disappear. The source links above allow the inspection to be repeated.
