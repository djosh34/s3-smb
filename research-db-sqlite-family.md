# Research: SQLite-family databases on S3 for the new engine

Ticket: [#573](https://github.com/djosh34/s3-smb/issues/573). Map: [#572](https://github.com/djosh34/s3-smb/issues/572).

Question: which SQLite-family databases can be the small metadata store of the new engine, with S3 as the only durable store?

Date: 2026-10-05.

## How to read this

Every claim carries one tag:

- **(doc)**: documented by the project. Linked.
- **(src)**: read from source at a pinned commit. Linked.
- **(inf)**: inferred by me. Not tested.

Nothing here was run against MinIO or Garage. The only thing I ran was `CGO_ENABLED=0 go build` of the Litestream core and S3 packages.

Pinned commits:

| Project | Version | Commit |
|---|---|---|
| Litestream | v0.5.17 (2026-08-31) | [`ccd326c`](https://github.com/benbjohnson/litestream/tree/ccd326c175b583b5e82893a6078f06dcef5fba3f) |
| superfly/ltx | v0.5.2 (used by Litestream) | [`434a99d`](https://github.com/superfly/ltx/tree/434a99d36f0311033915820a5a53d2eafd32a559) |
| LiteFS | v0.5.14 (2025-04-22) | [`a51e72d`](https://github.com/superfly/litefs/tree/a51e72d84eeb96ad210ef407230161192120ab96) |
| libSQL (bottomless) | main | [`d6c75af`](https://github.com/tursodatabase/libsql/tree/d6c75af6353bb1c34985399608e37cd272a35aa1) |
| Turso (Rust rewrite) | main | [`f2d7186`](https://github.com/tursodatabase/turso/tree/f2d71863e3a7863f0c61d397364945f9d9e46b5e) |
| Graft | v0.2.1 (2025-12-04) | [`f38e035`](https://github.com/orbitinghail/graft/tree/f38e03536d106747132476d7f21b84ffa64535c6) |

Backend facts are taken from the ticket and from [research-s3-guarantees.md](https://github.com/djosh34/s3-smb/blob/research/s3-guarantees/research-s3-guarantees.md). In short: Garage has no conditional writes and ignores `If-None-Match` on PUT. MinIO has `If-Match` and `If-None-Match` on PUT only.

## Short answer

- **Only Litestream is a real option.** It is Go, pure Go in its core, maintained, and runs on Garage. It has a call, `DB.SyncAndWait`, that returns only after the commit is in S3. That fits FLUSH.
- **Litestream does not give us safety for free.** It assumes one writer and does not stop a second one. It has no fence. Its object keys are named by transaction number, so a writer that lost its local disk and reuses a number writes the same key again. A late PUT can then swap in an old file, and restore does not check that files chain together. The result can be a silently wrong database. This is the same late-request problem as the parked effort, just in a smaller corner.
- **Those corners can be closed with a few dumb rules on our side.** Always restore from S3 at start, never trust a leftover local DB, run one server only, and treat a failed FLUSH as fatal. They are listed at the end. They are rules, not a fencing design.
- **LiteFS is out.** It has no S3 backend. Its S3 backup was LiteFS Cloud, which shut down in October 2024. It also needs FUSE and Consul.
- **libSQL bottomless is out.** It is Rust inside the `sqld` server, has had no real change since early 2025, and the project tells new users to go to Turso. Turso's open-source engine has no S3 backend. Its S3 storage is in the closed Turso Cloud.
- **Graft is out for now.** It is alpha, Rust, and needs a loadable SQLite extension plus cgo from Go. Its safety rests on `If-None-Match: *` for commits, which Garage ignores. It has no GC of old segments.
- **No other maintained Go SQLite-on-S3 store exists** that I could find. Pure Go VFS toolkits exist, but they leave the S3 part, and all its safety, to us.

## Summary table

| | Litestream v0.5.17 | LiteFS v0.5.14 | libSQL bottomless | Graft v0.2.1 |
|---|---|---|---|---|
| Needs conditional writes | No for replication. The optional lease does. | n/a, no S3 backend | No | Yes, `If-None-Match: *` on each commit |
| Runs on Garage | Yes (doc) | No | Yes in theory | Unsafe: the guard is ignored (inf) |
| Needs local disk | Yes, the live DB and LTX files | Yes | Yes | Yes, a fjall store |
| Synchronous commit to S3 | `DB.SyncAndWait`, one PUT (src) | No | `wait_until_committed` in Rust (src) | `volume_push`, two PUTs (src) |
| Overwrites in place | No in normal use. Same key again after a TXID reuse (inf) | n/a | `.meta`, `.changecounter`, `.dep`, tombstone (src) | No for commits. Segments are new keys (src) |
| Stops a second writer | No. "Your responsibility" (doc) | Consul lease | No | Only with conditional PUT |
| Own cleanup | Yes, retention and compaction delete old LTX files (src) | n/a | Yes, by generation | None found (src) |
| Language | Go, pure Go core | Go, FUSE | Rust | Rust, SQLite extension |
| License | Apache-2.0 | Apache-2.0 | MIT | Apache-2.0 or MIT |
| Status 2026 | Active, releases every few weeks | Beta, last release 2025-04 | Maintenance only | Alpha, last release 2025-12 |
| Read back without s3-smb | `litestream restore`, then `sqlite3` | n/a | `bottomless-cli` | Graft extension |
| Verdict | Candidate, with rules | Out | Out | Out |

## Litestream

Litestream ships SQLite WAL changes as LTX files to object storage. Version 0.5 replaced the old "generations" layout with LTX files and a compaction ladder.

### How it works, in brief

- The app writes a normal SQLite file on local disk in WAL mode. Litestream holds a read transaction to stop checkpoints, reads new WAL frames, and writes them to a local LTX file per sync. (src: [`db.go`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/db.go))
- `Replica.Sync` uploads each local L0 file whose TXID is above the remote position, in order. (src: [`replica.go#L164-L234`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/replica.go#L164-L234))
- The S3 key is `<path>/<level as 4 hex digits>/<minTXID>-<maxTXID>.ltx`. (src: [`s3/replica_client.go#L718-L724`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/s3/replica_client.go#L718-L724))
- Compaction merges L0 into L1, L2, L3 at 30 s, 5 min, 1 h by default. A daily snapshot level holds a full copy. (src: [`compaction_level.go#L14-L19`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/compaction_level.go#L14-L19), [`store.go#L60-L72`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/store.go#L60-L72))
- Restore lists files, builds a plan, and merges them into one SQLite file. (src: [`replica.go#L686-L790`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/replica.go#L686-L790))

### 1. Conditional writes and Garage

- Replication uses plain PutObject, GetObject, ListObjectsV2 and DeleteObject. No conditional headers. (src: grep of `IfMatch`/`IfNoneMatch` finds them only in `s3/leaser.go`)
- Garage is listed with a config example in [`docs/PROVIDER_COMPATIBILITY.md`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/docs/PROVIDER_COMPATIBILITY.md). (doc)
- There is an S3 `Leaser` that uses `If-None-Match`, `If-Match` on PUT, and `If-Match` on DELETE. (src: [`s3/leaser.go#L171-L270`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/s3/leaser.go#L171-L270)) It is not wired into replication: only its tests call `NewLeaser`. (src) Wiring it in is an open feature request, [#1301](https://github.com/benbjohnson/litestream/issues/1301). It cannot work on Garage, and its release step needs DELETE with `If-Match`, which MinIO lacks.

### 2. Local disk

- Required. The live database, its WAL and the local LTX files are all on local disk. (src)
- Wiped while running: every commit not yet uploaded is lost. If FLUSH waits for `SyncAndWait`, nothing that was acknowledged by FLUSH is lost. (inf)
- Wiped and then restarted: the app must restore from S3 before opening. The library example does this only if the file is missing. (src: [`_examples/library/s3/main.go`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/_examples/library/s3/main.go))
- **Danger: a stale local DB wins.** If the local DB is behind the replica at start, Litestream does not refuse. It drops its local L0 files, fetches the newest remote L0 file as a baseline, and the next sync takes a snapshot "at the current database state". (src: [`db.go#L1583-L1660`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/db.go#L1583-L1660)) So an old local file becomes the newest state in S3. Flushed data rolls back silently. (inf) For us this means: delete any local DB at start and always restore from S3.

### 3. Synchronous commit

- `DB.SyncAndWait(ctx)` runs `DB.Sync` (WAL to local LTX) and then `Replica.Sync` (upload). It returns only when both are done. (src: [`db.go#L714-L728`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/db.go#L714-L728))
- Cost per FLUSH: a WAL read, a local LTX write with fsync, then one PUT per pending L0 file. If the background loop already made several L0 files, they upload one after another. (src)
- Latency: about one small PUT plus local fsync. On a LAN MinIO or Garage this is tens of milliseconds. (inf, not measured)
- Concurrency: only one replica sync runs at a time. Other callers wait on a semaphore. (src: [`replica.go#L236-L246`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/replica.go#L236-L246))
- Default behaviour without our call is async, every second. The docs say a crash loses that window. (doc: [litestream.io/tips](https://litestream.io/tips/))
- On a failed upload, the replica position is reset and recomputed from S3 next time. (src: [`replica.go#L170-L178`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/replica.go#L170-L178))

### 4. Overwrites, deletes and late requests

What it writes and deletes:

- **New keys only, in normal single-writer use.** Every L0 file has a fresh TXID range. A retry of the same upload writes the same bytes to the same key. (src, inf)
- **Compaction** writes a new key covering a TXID range. A rerun after a crash writes the same range again with the same pages. (inf)
- **Deletes:** L0 files after they are in L1 and older than 5 minutes; higher levels by TXID; snapshots older than retention. It always keeps at least one file per level. (src: [`compactor.go#L235-L360`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/compactor.go#L235-L360))
- `DeleteAll` deletes everything under its path prefix. (src: [`s3/replica_client.go`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/s3/replica_client.go#L1282-L1300))

Late requests:

- **Late PUT, same content.** Harmless. (inf)
- **Late PUT after a TXID reuse.** This is the dangerous case. Sequence: commit N+1 is written locally, its upload times out with unknown outcome, the process dies, the disk is lost. A new start restores N from S3 and writes a different N+1. Then the old PUT lands and replaces it. Restore checks that TXIDs are contiguous, and each file checks its own checksum. It does **not** check that one file's post-apply checksum equals the next file's pre-apply checksum. Restore also forces the "no checksum" flag. (src: [`ltx compactor.go#L89-L104`](https://github.com/superfly/ltx/blob/434a99d36f0311033915820a5a53d2eafd32a559/compactor.go#L89-L104), [`replica.go#L746`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/replica.go#L746)) The result is a database built from pages of two histories. `PRAGMA integrity_check` may catch broken b-trees, but not wrong but well-formed rows. (inf)
- **Late DELETE.** Litestream deletes only files it no longer needs, so a late DELETE of such a key does nothing new. After a TXID reuse, a late DELETE could remove the new L0 file. Restore then fails loudly with a gap, unless L1 already covers it. (inf)
- **Timeouts.** The SDK retries. On error the sync returns an error and the caller decides. There is no "did it land" check; the next sync recomputes the remote position by listing. (src)

How likely is the TXID reuse case for us? It needs an unacknowledged upload, a crash, a lost local disk and a restart, all before the stale request lands. With default AWS SDK timeouts the window is seconds. It is rare. It is not impossible, and the result is silent. (inf)

How to make it impossible: never reuse a TXID range. One simple way is to force a fresh snapshot after every restore, so a new process never writes the same small L0 key twice. I did not find an option for this. It would need a check in a prototype. (inf)

### 5. Two writers

- Not prevented. The docs: "It is *your* responsibility to ensure you do not have multiple applications replicating concurrently." And: "Multiple applications replicating into the same bucket & path can cause situations where you will be unable to restore." (doc: [litestream.io/tips](https://litestream.io/tips/))
- Two writers write the same keys with different content. (inf)
- The leaser is not used, see point 1. Even if it were, it is a time lease. Uploads do not carry a fence token, so a paused old holder can still write after its lease ran out. (src, inf)

### 6. GC of our chunks

- Litestream only touches keys under its own path. Our chunks must live under a different prefix, because `DeleteAll` wipes its whole prefix. (src)
- Our GC can read the live local DB, since there is only one writer. It lists chunk keys and deletes those not referenced and older than a grace period. (inf)
- If we ever want point-in-time restore, the chunk grace period must be longer than Litestream's retention. Otherwise an old DB version points at deleted chunks. (inf)
- A chunk referenced by a commit that was not yet uploaded is safe from GC, because GC reads the local DB, which already has it. (inf)

### 7. Maturity

- Started 2020. About 14,400 stars. Apache-2.0. (doc: GitHub)
- Releases v0.5.11 to v0.5.17 between April and August 2026. Active. (doc: GitHub releases)
- Core is pure Go: it imports `modernc.org/sqlite`, and `CGO_ENABLED=0 go build ./ ./s3` works. The VFS needs cgo and a build tag. (src, ran)
- Dependencies of the core and S3 packages: about 180 packages, including AWS SDK v2, Prometheus and modernc libc. The module also pulls Azure, GCS, NATS and SFTP clients into `go.mod`, but not into our binary. (src, ran `go list -deps`)
- **Open corruption bugs (2026):**
  - [#1164](https://github.com/benbjohnson/litestream/issues/1164): restored DB is malformed, random, in production.
  - [#1220](https://github.com/benbjohnson/litestream/issues/1220): restore corruption reproduces in short soak tests on main, including the MinIO soak.
  - [#1309](https://github.com/benbjohnson/litestream/issues/1309): restored DB fails `integrity_check` when the first snapshot is slow.
  - [#1490](https://github.com/benbjohnson/litestream/issues/1490): snapshot compaction stalls forever after a frame offset bug.
  - [#1427](https://github.com/benbjohnson/litestream/issues/1427): restore rejects valid checksum-tracked LTX files.

  These are the biggest risk. The 0.5 line is young and its restore path has known bugs. Our metadata DB would be small and write little, which avoids the slow-snapshot case of #1309, but #1220 has no known cause. (inf)

### 8. Read back without s3-smb

- `litestream restore -o out.db s3://bucket/path` gives a plain SQLite file. Then any `sqlite3` works. (doc)
- The LTX format is documented in [`docs/LTX_FORMAT.md`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/docs/LTX_FORMAT.md) and the `ltx` CLI. (doc)

### Litestream VFS write mode

Litestream also has a VFS that reads pages straight from S3 and can write. Not a fit:

- Marked experimental and needs cgo. (doc: [`docs/VFS.md`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/docs/VFS.md))
- Its conflict check lists the remote and then uploads. That is check-then-write, not compare-and-swap. (src: [`vfs.go#L2064`](https://github.com/benbjohnson/litestream/blob/ccd326c175b583b5e82893a6078f06dcef5fba3f/vfs.go#L2064))
- Open bugs: [#1271](https://github.com/benbjohnson/litestream/issues/1271) "database disk image is malformed" after sync, [#1018](https://github.com/benbjohnson/litestream/issues/1018) write buffer race.

## LiteFS

- A FUSE file system that ships LTX files between nodes. (doc: [README](https://github.com/superfly/litefs/blob/a51e72d84eeb96ad210ef407230161192120ab96/README.md))
- **No S3 backend.** Backup types are `file` and `litefs-cloud` only. (src: [`cmd/litefs/config.go#L185-L194`](https://github.com/superfly/litefs/blob/a51e72d84eeb96ad210ef407230161192120ab96/cmd/litefs/config.go#L185-L194))
- LiteFS Cloud was deprecated in July 2024 and ran until 2024-10-15. (doc: [Fly.io community](https://community.fly.io/t/sunsetting-litefs-cloud/20829)) A third-party replacement, [stephen/litefs-backup](https://github.com/stephen/litefs-backup), had its last push in 2024-12. (doc: GitHub)
- Needs FUSE, a Consul lease or a static primary, and local disk. (doc)
- Status beta. Last release v0.5.14 on 2025-04-22. (doc)
- Questions 1 to 8 do not apply. **Out.**

## libSQL bottomless, and Turso

### Bottomless

- Rust crate inside libSQL, used by the `sqld` server. It ships WAL frames and snapshots to S3. (src: [`bottomless/src/replicator.rs`](https://github.com/tursodatabase/libsql/blob/d6c75af6353bb1c34985399608e37cd272a35aa1/bottomless/src/replicator.rs))
- Conditional writes: none used. (src: no `if_match` or `if_none_match` in `bottomless/src`)
- Synchronous: `wait_until_committed(frame_no)` exists, in Rust. (src: [`replicator.rs#L674`](https://github.com/tursodatabase/libsql/blob/d6c75af6353bb1c34985399608e37cd272a35aa1/bottomless/src/replicator.rs#L674))
- Overwrites in place: `<db>-<generation>/.meta`, `.changecounter`, `.dep`, and `<db>.tombstone`. (src: [`replicator.rs#L1919-L1975`](https://github.com/tursodatabase/libsql/blob/d6c75af6353bb1c34985399608e37cd272a35aa1/bottomless/src/replicator.rs#L1919-L1975))
- Generations are UUIDv7, so ordered by time. Restore picks the newest generation. A stale server that starts a new generation would become "newest". (src, inf)
- Two writers: not prevented. (inf)
- Go: there is no Go API to bottomless. `go-libsql` is a cgo client for embedded replicas that sync from a `sqld` server, not from S3. (inf)
- Activity: last change under `bottomless/` is 2025-01. The README says libSQL is "actively maintained, but new features are being developed in Turso". (doc: [README](https://github.com/tursodatabase/libsql/blob/d6c75af6353bb1c34985399608e37cd272a35aa1/README.md))
- **Out**: Rust server, maintenance only, no Go embedding.

### Turso (the Rust rewrite)

- Beta. (doc: libSQL README)
- The open-source engine has no S3 dependency. (src: no `aws-sdk-s3` or `object_store` in any `Cargo.toml` at [`f2d7186`](https://github.com/tursodatabase/turso/tree/f2d71863e3a7863f0c61d397364945f9d9e46b5e))
- There is a `DurableStorage` hook for MVCC, and a user who built an object-store backend on it found undocumented failure semantics. (doc: [turso#9512](https://github.com/tursodatabase/turso/issues/9512))
- S3 as the durable store exists only in the closed Turso Cloud. (doc: [Turso blog](https://turso.tech/blog/turso-cloud-goes-diskless))
- **Out.**

## Graft

- Transactional page store on object storage, with a SQLite VFS shipped as a loadable extension. Alpha: "please contact @carlsverre before using it in production". (doc: [README](https://github.com/orbitinghail/graft/blob/f38e03536d106747132476d7f21b84ffa64535c6/README.md))

1. **Conditional writes.** Each commit is `put_opts` with `PutMode::Create`, using `S3ConditionalPut::ETagMatch`, which sends `If-None-Match: *`. (src: [`remote.rs#L127-L130`](https://github.com/orbitinghail/graft/blob/f38e03536d106747132476d7f21b84ffa64535c6/crates/graft/src/remote.rs#L127-L130), [`remote.rs#L205-L222`](https://github.com/orbitinghail/graft/blob/f38e03536d106747132476d7f21b84ffa64535c6/crates/graft/src/remote.rs#L205-L222)) On MinIO this works. On Garage the header is ignored, so a duplicate commit overwrites instead of failing, and divergence goes unseen. (inf)
2. **Local disk.** A fjall store holds local commits until pushed. Wiped: unpushed commits are lost. (src, inf)
3. **Synchronous.** `volume_push` uploads a segment, then the commit object. Two sequential PUTs. A pending-commit record handles unknown outcomes on restart by reading the remote commit. (src: [`rt/action/remote_commit.rs#L32-L110`](https://github.com/orbitinghail/graft/blob/f38e03536d106747132476d7f21b84ffa64535c6/crates/graft/src/rt/action/remote_commit.rs#L32-L110))
4. **Overwrites.** Commits are create-only. Segments get fresh IDs. Nothing overwritten on MinIO. (src)
5. **Two writers.** The create-only commit makes the loser fail. That needs conditional PUT. (src)
6. **Cleanup.** No delete call in `remote.rs`. No GC of old segments found. Storage grows forever. (src)
7. **Maturity.** Started 2024. About 1,550 stars. Last release v0.2.1 on 2025-12-04. Since then, only dependency and docs commits. Rust. From Go it needs cgo SQLite with extension loading and a Rust-built shared library. (doc, src)
8. **Read back.** Only with the Graft extension or crate. (inf)

**Out**: alpha, not Go, unsafe on Garage, no GC.

## Other options looked at

| Option | What it is | Why not |
|---|---|---|
| [Verneuil](https://github.com/backtrace-labs/verneuil) | Rust VFS, async copy to S3 as content-addressed chunks plus a manifest | Async only. Copies are read replicas. The manifest is overwritten per DB, so a late PUT can roll it back. (doc, inf) |
| [ncruces/go-sqlite3/vfs](https://pkg.go.dev/github.com/ncruces/go-sqlite3/vfs), [gosqlite.org/vfs](https://pkg.go.dev/gosqlite.org/vfs), [psanford/sqlite3vfs](https://github.com/psanford/sqlite3vfs) | Go toolkits to write your own VFS | No S3 backend shipped. We would own the whole commit protocol, which is what #572 wants to avoid. (doc) |
| [PDOK/go-cloud-sqlite-vfs](https://github.com/PDOK/go-cloud-sqlite-vfs) | Go VFS for Azure and GCS | No S3, 4 stars. (doc) |
| [stephen/litefs-backup](https://github.com/stephen/litefs-backup) | Replacement for LiteFS Cloud | Needs LiteFS. Inactive since 2024-12. (doc) |

## If Litestream is chosen: the rules it needs

These come from the failure modes above. Each is a plain rule, not a protocol.

1. **Restore on every start.** Delete any local DB, WAL and `-litestream` dir, then restore from S3. Never trust local state. This closes the stale-local-DB rollback. (inf)
2. **FLUSH calls `SyncAndWait`.** If it fails, stop accepting writes and exit. The restart restores from S3. Do not keep running with an unknown upload. (inf)
3. **One server per bucket.** Litestream cannot enforce it, and Garage cannot either. This is the open decision in [#578](https://github.com/djosh34/s3-smb/issues/578). (doc, inf)
4. **Close the TXID reuse window.** After a restore, wait longer than the S3 client's total timeout before the first new upload, or make the first upload a full snapshot so no small L0 key is reused. The second needs a prototype to confirm it is possible. (inf)
5. **Chunks under their own prefix.** GC reads the local DB and keeps a grace period. (inf)
6. **Restore with `integrity_check`, and test restore often.** The open restore-corruption bugs (#1164, #1220, #1309) are the biggest risk. A prototype should run the Litestream soak tests against MinIO and Garage and inject a crash after every S3 request. (inf)

## Open questions for a prototype

- Does `SyncAndWait` latency on Garage and MinIO fit Time Machine's FLUSH pattern?
- Can a restart be forced to write a full snapshot as its first upload?
- Does #1220 reproduce with a small DB and few writes, like ours?
- What does a late PUT of a reused L0 key do to restore in practice? Silent mix, or loud failure?
