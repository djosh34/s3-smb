# Research: key-value and LSM databases on S3 for the new engine

Ticket: [#574](https://github.com/djosh34/s3-smb/issues/574). Map: [#572](https://github.com/djosh34/s3-smb/issues/572).

Date: 2026-10-05.

Question: which key-value or LSM database can be the small metadata store of the new engine, with S3 as its only durable store?

## How to read this

Every claim carries a tag:

- **(doc)**: written in the project's own documentation, README, RFC or issue tracker.
- **(src)**: read from source code at the pinned commit, with a link.
- **(inferred)**: my own reasoning from the docs and source. Not tested.

Nothing here was run against MinIO or Garage. All latency numbers are inferred.

Pinned commits:

| Project | Version | Commit | Date |
| --- | --- | --- | --- |
| SlateDB | v0.17.0 | [`c1e36fc`](https://github.com/slatedb/slatedb/tree/c1e36fc07fb9b759d31484dda18c1c1aad8a2853) | 2026-09-29 |
| ZeroFS | v2.3.5 | [`4283dad`](https://github.com/Barre/ZeroFS/tree/4283dadfd2c6053d7d7ea04742f0d8465065f596) | 2026-09 |
| Pebble | v2.1.7 | [`66d468e`](https://github.com/cockroachdb/pebble/tree/66d468e449bbd52ddf9e53c3a7cdec37e885aaee) | 2026-09 |
| IsleDB | v0.12.2 | [`e0c8fbd`](https://github.com/ankur-anand/isledb/tree/e0c8fbdb01ba38c311b79726987229b52e14b1a8) | 2026-10-05 |

Backend facts taken as given from the ticket: Garage has no conditional writes. MinIO has `If-Match` and `If-None-Match` on PUT only, not on DELETE or COPY.

## Short answer

1. **No candidate runs safely on Garage.** Every maintained S3-only database found relies on conditional PUT for its commit or for fencing. On Garage the headers are ignored, so the safety silently goes away. Only an external lock (ZeroFS uses Redis) can stand in, and that lock has the same late-request hole that parked #173.
2. **On MinIO, two candidates fit: SlateDB and IsleDB.** Both need only conditional PUT, which MinIO has. Both keep S3 as the only durable store and need no local disk for safety.
3. **IsleDB has the simplest commit rule:** one root object `manifest/CURRENT`, always replaced with `If-Match`. A late PUT of that root fails with 412. It is pure Go. But it is 9 months old, has one author, about 24k lines of non-test Go, and few users.
4. **SlateDB is the mature one** (2.5 years, 3.4k stars, foundation-style governance, used by ZeroFS and others). But its commit rule is "create the next numbered file if absent", and that rule breaks when GC deletes old names. SlateDB patched this twice in 2026 (fence files never GC'd by default, and a `.boundary` file updated with `If-Match`). Its Go binding is cgo over a Rust shared library, and the API still changes every month.
5. **Pebble is out.** Its WAL, MANIFEST and remote-object catalog live on a local filesystem. S3 holds only SSTables.
6. **ZeroFS is a strong reference, not a library.** It is almost the design #572 describes: metadata in SlateDB, file data in immutable segment objects, and its own GC. But it is AGPL-3.0 (same as s3-smb), a whole filesystem server, uses a fork of SlateDB, and needs a local cache disk.
7. **None of them can be read back with standard tools.** Each needs its own library or CLI.

## Summary table

| | SlateDB (+ Go binding) | IsleDB | ZeroFS | Pebble shared storage |
| --- | --- | --- | --- | --- |
| Language for us | Rust via cgo and a `.so` | Pure Go | Rust server; Go client talks 9P | Pure Go |
| Needs on S3 | PUT `If-None-Match` and PUT `If-Match` | PUT `If-Match` and `If-None-Match` | Same as SlateDB, or Redis | Nothing special, S3 is not the root |
| MinIO | Yes (inferred) | Yes (inferred) | Yes, it probes at startup (src) | Not applicable |
| Garage | Unsafe: runs, but fencing and commit are silently lost (inferred) | No (doc) | Only with Redis lock (doc) | Not applicable |
| CAS swappable | In Rust yes (`ObjectStore` trait). From Go no | Not without forking | Yes, Redis, with a lock-expiry hole | Not applicable |
| Local disk | Not needed. Optional cache only | Readers need a cache dir. Writer does not (inferred) | Required cache dir | Required for WAL and MANIFEST |
| Sync commit to S3 | `WriteHandle.AwaitDurable` or `Flush`: one WAL PUT | `Flush` or `WaitCommitted`: SST PUT + CURRENT PUT | fsync = segment PUT + L0 SST PUT + manifest PUT | No |
| Objects overwritten in place | Two boundary files (`If-Match`) | `manifest/CURRENT` (`If-Match`), `maintenance/HEAD` | Same as SlateDB | Local files |
| Late PUT on root | Name already used: 412. Name GC'd: the boundary check rejects it | 412 because the ETag moved | Same as SlateDB. With Redis: can overwrite | Not applicable |
| Own retry on unknown outcome | Unbounded retries, verifies its own PUT by a metadata ID | Retries, checks for its own receipt before retrying | Same as SlateDB | Not applicable |
| Two writers | Epoch in manifest plus create-only WAL and fence files | Writer fence epoch in `CURRENT` | SlateDB fencing plus writer epoch | One process, local lock |
| Own GC | Yes, by `min_age` (default 5 min) | Yes, durable GC plans with not-before times | Yes, full segment reclaim | Yes |
| Age, stars | 2.5 y, 3.4k | 9 mo, 49 | 15 mo, 3.1k | 6 y, used by CockroachDB |
| License | Apache-2.0 | Apache-2.0 | AGPL-3.0 or commercial | BSD-3 |
| Read back without s3-smb | `slatedb` CLI `scan` | IsleDB Go library only | Run ZeroFS and mount | Not applicable |

## 1. SlateDB and its Go binding

SlateDB is an embedded LSM key-value store in Rust that "depends on object storage alone for durability" (doc, [slatedb.io](https://slatedb.io/)).

### 1.1 Conditional writes, Garage, swapping the CAS

- **WAL SSTs are written with create-if-absent.** `write_create` uses `PutMode::Create` and maps `AlreadyExists` to `Fenced` (src, [slatedb/src/wal/slatedb/store.rs#L222](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/wal/slatedb/store.rs#L222)). This is `If-None-Match: *` on S3.
- **Manifest and compaction-state files are numbered and created with create-if-absent.** "The filename is the commit point: a writer creates the next file with a create-if-absent operation, and success means the sequenced update won" (doc, [RFC 0026](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/rfcs/0026-garbage-collector-boundary.md)). Source: `write_unchecked` (src, [slatedb-txn-obj/src/object_store.rs#L369](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb-txn-obj/src/object_store.rs#L369)).
- **Boundary files are updated with `If-Match`.** `advance` uses `PutMode::Update(version)` (src, [slatedb-txn-obj/src/object_store.rs#L296](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb-txn-obj/src/object_store.rs#L296)). They can be turned off with `boundary_files_enabled = false`, documented as "Disable this only for object stores that do not support conditional overwrites" (src, [slatedb/src/config.rs#L1656](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/config.rs#L1656)).
- **The Go binding always asks for `ETagMatch` conditional puts** on S3 (src, [bindings/uniffi/src/object_store_builder.rs#L208](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/bindings/uniffi/src/object_store_builder.rs#L208)).
- **MinIO:** SlateDB needs only conditional PUT, never conditional DELETE or COPY. I found no `copy` or `rename` call in the engine (src, grep of `slatedb/src`). So it should work on MinIO. **(inferred)**
- **Garage:** Garage ignores `If-None-Match` on PUT (backend fact from the ticket). SlateDB has no startup probe for this; ZeroFS added its own (src, [zerofs/src/storage_compatibility.rs#L41](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/zerofs/src/storage_compatibility.rs#L41)). So SlateDB on Garage would start, run, and look fine. But every create-if-absent becomes a blind overwrite. **(inferred)** This matters even with one process: the writer and the in-process compactor both write the next manifest file. The compactor is on by default (src, [slatedb/src/config.rs#L1116](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/config.rs#L1116)). If both write manifest `N+1`, one update is lost with no error. A lost writer update can drop an L0 SST reference, whose WAL is then GC'd. That is silent data loss. **(inferred)**
- **Maintainers on Garage-like stores:** the ZeroFS author asked; the answer after talking to the SlateDB lead was "won't fix" (doc, [ZeroFS#196](https://github.com/Barre/ZeroFS/issues/196)). The SlateDB lead's stance on stores without `If-Match` is to disable boundary files and set a long `min_age` (doc, [slatedb#1818](https://github.com/slatedb/slatedb/issues/1818)).
- **Swapping the CAS:** in Rust, yes. SlateDB takes any `object_store::ObjectStore`, and ZeroFS wraps it with a Redis lock (src, [zerofs/src/redis_conditional_store.rs](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/zerofs/src/redis_conditional_store.rs)). From Go, no: the uniffi binding exposes no foreign object store callback, only merge operators, filter policies, logging and metrics (src, [bindings/uniffi/src](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/bindings/uniffi/src)). **(src)** We would have to write Rust.

### 1.2 Local disk

- Not needed. The object store cache is optional and only on when `object_store_cache_options.root_folder` is set (src, [slatedb/src/config.rs#L816](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/config.rs#L816)).
- If the cache disk is wiped while running, only cached reads are lost. **(inferred)** The memtable and unflushed WAL buffer are in RAM, not on disk.
- Without a disk cache, every cold read is a GET. With a few MB of path-to-chunk metadata this is fine; the block cache in RAM holds it. **(inferred)**

### 1.3 Synchronous commit

- Since v0.17, `put` "does not wait for the write to become durable in object storage. Call `WriteHandle::await_durable`" (src, [slatedb/src/db.rs#L2152](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/db.rs#L2152)). The Go binding exposes `WriteHandle.AwaitDurable` and `Db.Flush` (src, [bindings/uniffi/src/write_handle.rs](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/bindings/uniffi/src/write_handle.rs)).
- Writes are group-committed: the WAL buffer is written every `flush_interval`, default 100 ms (src, [slatedb/src/config.rs#L1104](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/config.rs#L1104)), or when full. `Flush` with `FlushType::Wal` writes it now (src, [slatedb/src/db.rs](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/db.rs)).
- So an SMB FLUSH can call `Flush` and reply after it returns. Cost: one PUT of the WAL SST. No manifest write on the hot path. **(inferred)** Latency is one small PUT on MinIO, likely 5 to 30 ms on a LAN, plus up to `flush_interval` if we wait for the timer instead. **(inferred, not measured)**

### 1.4 Overwrites, deletes, late requests, retries

- **Overwritten in place:** only the two boundary files (`gc/manifest.boundary`, `gc/compactions.boundary`), always with `If-Match` (doc, RFC 0026). Everything else is written once under a new name.
- **Deleted:** old WAL SSTs, compacted SSTs, manifests and compaction files, by the GC after `min_age` (default 300 s, src, [slatedb/src/garbage_collector.rs#L59](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/garbage_collector.rs#L59)). Zero-byte WAL fence files are not GC'd unless enabled, with a data-loss warning (src, [slatedb/src/config.rs#L1619](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/config.rs#L1619)).
- **Late PUT of a numbered file whose name still exists:** gets 412 and is harmless. **(inferred)**
- **Late PUT of a numbered file whose name was GC'd:** this is exactly the parked effort's late-request problem, and SlateDB hit it. The RFC: "create-if-absent only protects against objects that currently exist. Once GC deletes a sequenced metadata object, create-if-absent no longer remembers that the ID was already used" (doc, RFC 0026). A user showed lost flushed writes this way with the WAL ([slatedb#1622](https://github.com/slatedb/slatedb/issues/1622), fixed in 0.14, July 2026). The fixes are: keep WAL fence files forever by default, and check a monotonic boundary file after each manifest write. With both in place, a stale writer's late file is rejected and left as an unreferenced object. **(doc + inferred)** Without `If-Match` (Garage) the boundary cannot be kept safe and the guard falls back to "`min_age` longer than any stalled process". **(doc, #1818)**
- **Late DELETE:** GC deletes only names that are no longer referenced and never reused by a live writer. A late DELETE cannot remove live data unless a name is reused, and the fences above exist to stop reuse. **(inferred)**
- **Late COPY:** SlateDB issues no COPY. **(src)**
- **Retries:** SlateDB wraps the store in `RetryingObjectStore`. Retries are unbounded by default (src, [slatedb/src/retrying_object_store.rs#L90](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/retrying_object_store.rs#L90)), configurable by `object_store_max_retries`. Each PUT carries a `slatedbputid` metadata value. If a retry hits `AlreadyExists` or `Precondition`, it HEADs the object and treats it as its own success if the ID matches (src, `#L30`, `#L133`). That handles "my first PUT landed but I lost the reply". The underlying `object_store` S3 client also retries on its own. **(src)**
- **Worst case:** with unbounded retries, an S3 outage makes `Flush` hang, not fail. We must put our own deadline around FLUSH. **(inferred)**

### 1.5 Two writers

- Opening a writer bumps an epoch in the manifest and writes a zero-byte fence WAL file at the next WAL ID (src, [slatedb/src/fence.rs](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb/src/fence.rs), `wal/slatedb/writer_init.rs`). The old writer's next WAL PUT then gets `AlreadyExists` and closes with `Fenced`. **(src)**
- This is real fencing, done by the database, with no lock service. It holds only with working `If-None-Match`. **(inferred)**
- An open bug: "Open can loop indefinitely ... compactor fencing CAS against a live previous compactor (20+ min observed)" ([slatedb#1970](https://github.com/slatedb/slatedb/issues/1970), open). **(doc)**

### 1.6 GC of our chunks, and SlateDB's own cleanup

- SlateDB GCs its own objects under its prefix. It never looks at other prefixes. **(inferred)** Our chunks would sit under a separate prefix.
- Our GC: open a checkpoint or snapshot, scan all chunk references, LIST the chunk prefix, delete chunks that are unreferenced and older than a grace period. Chunk IDs must never be reused, so a late chunk PUT after delete only recreates an orphan that the next pass removes. **(inferred)** SlateDB has checkpoints (doc, [RFC 0004](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/rfcs/0004-checkpoints.md)) which give a stable view to scan.
- The hard part is the race between "chunk uploaded" and "reference committed". A grace period by chunk age covers it, as ZeroFS does with a 60 s floor (doc, [ZeroFS space reclamation](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/documentation/src/app/space-reclamation/page.mdx)). **(inferred)**

### 1.7 Maturity, size, license, maintenance

- First commit 2024-04, v0.1.0 2024-08, v0.17.0 2026-09-29. A minor release every month in 2026. Apache-2.0. 3.4k stars. **(doc, git tags)**
- Pre-1.0. The write API changed in 0.17 (`put` stopped waiting for durability). **(src)** Expect breaking changes each release.
- Correctness bugs fixed in the past 12 months include: "slatedb can lose flushed writes" (#1622), "GC deletes things it shouldn't" (#604), "Manifest merge can resurrect pruned external SST IDs" (#1935), "SequenceTracker ... silently corrupts sequence numbers" (#2024), "reads return stale results when a key straddles multiple SSTs" (#1367), "Resumed compaction can skip merge entries" (#1769). **(doc, issue tracker)** They are found and fixed fast, with DST and a formal model (doc, RFC 0026 "Testing"). But the rate is high for a store we want to run for years.
- Open bugs touching correctness: #2097 (non-repeatable reads in snapshots with remote durability), #1147, #1970. **(doc)**
- **Go binding:** `slatedb.io/slatedb-go`, generated by `uniffi-bindgen-go`, uses cgo and "links against the `slatedb_uniffi` shared library" which must be on the loader path (doc, [bindings/go/README.md](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/bindings/go/README.md)). The crate also builds a `staticlib` (src, [bindings/uniffi/Cargo.toml](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/bindings/uniffi/Cargo.toml)), so a static link should be possible with our own cgo flags. **(inferred)** It runs its own multi-threaded Tokio runtime inside our process (doc). Go binding releases since 2026-04 (v0.12.0). s3-smb already builds with `CGO_ENABLED=1` (src, `test/Dockerfile` in this repo), but we would add a Rust toolchain to the build for both architectures. **(inferred)**

### 1.8 Reading data without s3-smb

- The `slatedb` CLI has `scan`, `read-manifest` and more (src, [slatedb-cli/src/args.rs](https://github.com/slatedb/slatedb/blob/c1e36fc07fb9b759d31484dda18c1c1aad8a2853/slatedb-cli/src/args.rs)). Any SlateDB binding (Python, Node, Java, Go) can open it read-only. Not plain JSON, not `aws s3 cp`. **(src)**

## 2. ZeroFS (reference)

A filesystem served over NFS, 9P and NBD, built on SlateDB. Read as a reference for the #572 design, not as a library.

- **Design:** "File contents are split into 32 KiB extents; each extent is compressed, encrypted, and packed as a frame into immutable segment objects (up to 256 MiB). Metadata (inodes, directory entries, and one 32-byte pointer per extent) lives in an LSM-tree database on the same object store." (doc, [README.md](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/README.md)). This is nearly the #572 plan.
- **CAS:** "ZeroFS requires conditional writes (put-if-not-exists) for fencing ... for stores that don't, set `conditional_put` to a Redis URL." (doc). At startup it checks both `If-None-Match` and `If-Match` and refuses stores that ignore them (src, [zerofs/src/storage_compatibility.rs#L41](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/zerofs/src/storage_compatibility.rs#L41)).
- **The Redis fallback has a late-request hole.** It takes a Redis lock, does HEAD, then a plain PUT, all under a lock deadline (src, [zerofs/src/redis_conditional_store.rs#L100](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/zerofs/src/redis_conditional_store.rs#L100), `#L133`). On deadline it drops the future and reports an error, but the PUT may already be on the wire. It can land after the lock expired and after another writer's PUT, and overwrite it. **(inferred)** This is the #566 problem in another form. Not a safe way to run on Garage.
- **Local disk:** a cache dir is required (`[cache]` is required, doc). It holds read caches only: "raw-parts cache" and "decoded-block cache" (doc, caching page). Wiping it loses cached reads, not data. **(inferred)**
- **SlateDB WAL off:** ZeroFS runs SlateDB with `wal_enabled: false` so metadata only becomes durable when its flush coordinator flushes, after the segment PUT (src, [zerofs/src/cli/server.rs#L587](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/zerofs/src/cli/server.rs#L587)). An fsync is: seal and PUT the open segment, then flush the memtable to an L0 SST and write a manifest (doc, durability page). Writes between fsyncs can be lost; up to 256 MiB of open buffer plus 4 x 256 MiB uploads in flight (doc). **(doc)**
- **Ordering lesson for us:** "A flush uploads the open data segment before flushing metadata that refers to it. A durable manifest therefore never points to an unuploaded segment." (doc). Our chunk PUT must also finish before the commit that names it. **(inferred)**
- **GC:** tombstones, live-byte counters per segment, repack, a 60 s delete floor, and a daily orphan sweep that LISTs `segments/` (doc, space reclamation page). Large and careful. Shows how much GC grows once segments are packed. With one chunk per object and no repack, ours can be much smaller. **(inferred)**
- **Uses a fork of SlateDB:** `git = "https://github.com/Barre/slatedb.git", rev = "20c14bb..."` (src, [zerofs/Cargo.toml](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/zerofs/Cargo.toml)).
- **Maturity:** created 2025-07, v2.3.5, 155 tags, 3.1k stars. Jepsen local-fs runs and deterministic simulation in CI (doc). Open: "Segment reads return EIO until restart after one corrupted ranged GET" ([ZeroFS#642](https://github.com/Barre/ZeroFS/issues/642)). A user lost access after R2 returned false 404s ([ZeroFS#586](https://github.com/Barre/ZeroFS/issues/586)). **(doc)**
- **License:** AGPL-3.0 or commercial (doc). s3-smb is also AGPL-3.0, so no conflict. **(src, this repo's LICENSE)**
- **Go:** the Go package is a 9P client to a running ZeroFS server, cgo over a Rust cdylib (doc, [zerofs/zerofs-ffi/bindings/go/README.md](https://github.com/Barre/ZeroFS/blob/4283dadfd2c6053d7d7ea04742f0d8465065f596/zerofs/zerofs-ffi/bindings/go/README.md)). Using ZeroFS would mean running it as a sidecar and storing Time Machine bands on it. That replaces our engine with a large Rust filesystem; the opposite of "small". **(inferred)**
- **Read back:** run ZeroFS and mount; data is encrypted. **(doc)**

## 3. Pebble with shared storage

- Pebble's `Experimental.RemoteStorage` plus `CreateOnShared` put new SSTables on remote storage (src, [`options.go#L714`](https://github.com/cockroachdb/pebble/blob/66d468e449bbd52ddf9e53c3a7cdec37e885aaee/options.go#L714)).
- The WAL, the MANIFEST and the remote object catalog stay on a local `vfs.FS`. The catalog doc: "Catalog is used to manage the on-disk remote object catalog" (src, [`objstorage/objstorageprovider/remoteobjcat/catalog.go`](https://github.com/cockroachdb/pebble/blob/66d468e449bbd52ddf9e53c3a7cdec37e885aaee/objstorage/objstorageprovider/remoteobjcat/catalog.go)).
- Pebble ships no S3 driver, only a `remote.Storage` interface with local and in-memory implementations (src, `objstorage/remote/`).
- So: local disk is required, and wiping it loses the whole database, because the root of truth is local. S3 is not the only durable store. **Out.** **(src + inferred)**
- The only way to use Pebble would be to back up the local directory to S3 ourselves; that is our own commit protocol again. **(inferred)**

## 4. Go-native options

### 4.1 IsleDB

An "embedded key-value database written in Go for Amazon S3, Google Cloud Storage, Azure Blob Storage, MinIO" (doc, [isledb.com](https://isledb.com/)). [github.com/ankur-anand/isledb](https://github.com/ankur-anand/isledb).

**1. Conditional writes.** `manifest/CURRENT` is "the only authoritative database head" and is "Updated with conditional writes" (doc, [docs/object-store-schema.md](https://github.com/ankur-anand/isledb/blob/e0c8fbdb01ba38c311b79726987229b52e14b1a8/docs/object-store-schema.md)). S3 path sends `IfMatch` or `IfNoneMatch: "*"` on PutObject (src, [blobstore/blobstore_cas.go#L88](https://github.com/ankur-anand/isledb/blob/e0c8fbdb01ba38c311b79726987229b52e14b1a8/blobstore/blobstore_cas.go#L88)). PUT only, so MinIO should work. **(inferred)** Garage: no. Local stores "cannot provide the atomic conditional writes that IsleDB relies on" (doc, README). Unknown providers fall back to HEAD then plain PUT, which is not atomic (src, [blobstore/blobstore.go#L420](https://github.com/ankur-anand/isledb/blob/e0c8fbdb01ba38c311b79726987229b52e14b1a8/blobstore/blobstore.go#L420)). On Garage it would use the S3 path, send the header, and Garage would ignore it. **(inferred)** Swapping the CAS would mean forking `blobstore`. **(inferred)**

**2. Local disk.** Readers require `CacheDir` (doc, [api.md](https://github.com/ankur-anand/isledb/blob/e0c8fbdb01ba38c311b79726987229b52e14b1a8/api.md)). Compaction uses `ScratchDir` (doc). The writer keeps memtables in RAM. Wiping either dir loses caches and in-flight compaction work, not committed data. **(inferred)**

**3. Sync commit.** "A successful `Flush`, background flush, or `Writer.Close` is the durability and visibility boundary" (doc, README). `WaitCommitted(ctx, seq)` returns `nil` (committed), `ErrFenced` ("was not committed and never will be"), or a context error ("is not known yet: its commit is still being retried and may land") (doc, [api.md](https://github.com/ankur-anand/isledb/blob/e0c8fbdb01ba38c311b79726987229b52e14b1a8/api.md)). A flush is one SST PUT plus one `CURRENT` PUT with `If-Match`. **(doc)** So two sequential PUTs per FLUSH, maybe 10 to 60 ms on MinIO. **(inferred, not measured)** Default background flush every 1 s (doc).

**4. Overwrites, deletes, late requests.**
- Overwritten: `manifest/CURRENT` (always `If-Match`) and `maintenance/HEAD` ("Bounded mutable object") (doc).
- Deleted: SSTs, snapshots, pages and GC plan files, by maintenance, after the pinned-view window (default 1 h) and "not-before" times (doc).
- Late PUT on `CURRENT`: it carries an old ETag, so it fails with 412 once any newer commit landed. This holds no matter what GC deleted. This is the cleanest property among the candidates. **(inferred)** It assumes MinIO's ETag for `CURRENT` changes on every commit; the content includes a sequence, so the MD5 ETag changes. **(inferred)**
- Late PUT of an SST: SSTs become visible only through `CURRENT`. A late SST is an unreferenced object; "Readers ignore such objects because they do not discover data by listing" (doc).
- Unknown outcome: "Each attempt first checks whether the previous one applied before its response was lost, so a commit never lands twice" (doc, [api.md](https://github.com/ankur-anand/isledb/blob/e0c8fbdb01ba38c311b79726987229b52e14b1a8/api.md)). Retries back off from the flush interval up to 30 s and never stop on storage errors (doc). The AWS SDK also retries on its own. **(inferred)**

**5. Two writers.** "One writer owns a database prefix at a time. Writer ownership is fenced across processes" (doc). The fence state lives in `CURRENT` (doc, schema). There is also a separate maintenance fence (doc).

**6. GC.** IsleDB reclaims its own objects through durable plan records under `manifest/gc/` (doc). Our chunks would need the same outside-scan design as with SlateDB. **(inferred)**

**7. Maturity.** Repo created 2025-12-30, first commit 2026-01-24, v0.12.2 released 2026-10-05. 49 stars. One author (two git identities, 240 commits). About 24k lines of non-test Go and 66k with tests. Depends on Pebble (block cache and SST format), Badger, AWS SDK, GCS, Azure and gocloud. Apache-2.0. **(src, git)** No issues or users I could find beyond the author. Few eyes on it. **(inferred)**

**8. Read back.** Only with the IsleDB Go library. Manifest snapshots are zstd JSON in an `ISLM` envelope; SSTs are binary (doc).

### 4.2 Others looked at and dropped

- **rivian/delta-go:** last push 2025-05-05. Its log store needs put-if-absent or DynamoDB. Unmaintained. **(doc, GitHub)**
- **apache/iceberg-go:** active, but a table format; commits need a catalog (REST, SQL or Glue) for the atomic swap. Not a KV store and not S3-only. **(inferred)**
- **Plain Go LSMs** (Badger, bbolt, Pebble without shared storage, HyphaDB, goLSM): local disk only.

## 5. What this means for #572

- **For Garage, no existing database gives safety by itself.** Either Garage support is dropped for the new engine, or safety needs an outside service, or we accept "one server, no fencing, long GC delays" and document it. That is for [#578](https://github.com/djosh34/s3-smb/issues/578). **(inferred)**
- **For MinIO only, two choices:**
  - **IsleDB:** pure Go, one CAS root, the simplest story for late requests. Risk: young, single author, large code base we would have to trust or vendor.
  - **SlateDB:** mature and well tested, with real-world users. Risk: cgo plus Rust in our build, monthly breaking changes, and a commit rule whose safety depends on GC never deleting a name a stale writer might still create. The project knows this and has patched it, but the patches use `If-Match`, and the class of bug has bitten twice in 2026.
- **Both need our own GC for chunks** next to them, with a grace period and never-reused chunk IDs. **(inferred)**
- **Neither is readable with standard tools.** A plain manifest dump for disaster recovery would be our own addition ([#581](https://github.com/djosh34/s3-smb/issues/581)). **(inferred)**
- **Worth noting for [#580](https://github.com/djosh34/s3-smb/issues/580):** IsleDB's core idea (immutable data objects plus one `If-Match` root object) is small enough to write ourselves for a few MB of metadata. That would avoid both a young dependency and a Rust one, but it needs `If-Match` too, so it does not help Garage. **(inferred)**

## 6. Open points not checked

- No run on MinIO or Garage. A throwaway prototype should confirm: SlateDB and IsleDB open and commit on MinIO; Garage silently accepts their conditional PUTs; FLUSH latency.
- Whether MinIO's `If-Match` on PUT is atomic under concurrent requests was not checked here.
- Memory use of the SlateDB Tokio runtime and IsleDB's Pebble block cache inside s3-smb.
