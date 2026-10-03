# Research: replace embedded JuiceFS with a minimal storage engine? (s3-smb, main @ac712a6)

Status: DONE (2026-10-03). Read-only. Builds were done in a detached worktree at /tmp/jfs-audit, which has since been removed. Facts carried over from research-panics.md, research-code-quality.md, research-storage-tests.md and research-smb-rewrite.md are cited to those files. "TM" means Time Machine.

## 1. What JuiceFS gives us today

**How we call it.** Our side touches JuiceFS in four places:
- `storage/runtime.go:20-102`: `meta.NewSQLite`, `chunk.NewCachedStore` with a fixed config (:42), two `OnMsg` callbacks (`DeleteSlice`, `CompactChunk`) behind the delete guard (:80-91), and `fs.NewFileSystem` with a throwaway Prometheus registry (:93).
- `storage/volume.go`: `object.NewS3`, `WithPrefix` and `NewEncrypted(NewRSAEncryptor)` (:175). `meta.Format` doubles as our volume identity (`format.json`).
- `backup/`: `vfs.BackupTo` (backup.go:166), `vfs.CleanupBackups` (:107), `meta.DumpedMeta`/`LoadMeta` (recovery.go:223).
- `smbfs`: `fs.FileSystem` path calls (Lstat, Stat, Open/Lopen, Create, Mkdir, Delete, Rename, Symlink), `fs.File` calls (Pread, Pwrite, Fsync, Flush, Truncate, Close), and `meta` directly for SetAttr, GetAttr, StatFS, Readdir, the four xattr calls, and Setlk/Getlk (7 sites, locks.go/fs.go:285).

| JuiceFS feature | Used? | Notes |
|---|---|---|
| Meta engine: SQLite through vendored xorm, WAL, `synchronous=FULL` | yes | Namespace, attrs, xattrs, chunk→slice map, sessions, plocks, trash, delslices |
| Chunk store: 64 MiB chunks, 4 MiB blocks (volume.go:34 `BlockSize: 4096` KiB), slices | yes | An overwrite adds a new slice. A read that sees ≥5 slices in a chunk starts compaction (meta/base.go:2133-2138, also at write :2189). Compaction re-uploads the chunk and fills gaps with zeros (README.md:107-111) |
| Disk cache (100 GiB default, CsExtend checksums) and memory buffer (300 MiB) | yes | Source of #91, #128, #144 |
| Compaction, trash (14 d), delayed slice deletion | yes | Gated by our `CheckMaintenance` hook (the 3 added files and 16 patched files in docs/vendored.md) |
| Background GC of uncommitted objects | **no** | There is no `gc`. Orphans from crashes leak (research-panics.md §5) |
| Encryption `AES256GCM_RSA` | yes (default on) | RSA-wrapped key per object, scrypt-protected PEM (docs/recovery.md:31-35) |
| Compression zstd/lz4 (cgo DataDog/zstd) | optional | The README quick start uses zstd. Source of #131 |
| JSON metadata dump/load | yes (our recovery) | Source of #92, #114, plus the Fatalf/typeFromString/dump panics |
| Object client (aws-sdk-v2 wrapper) plus our `PutIfAbsent` and `NoSuchKey` mapping | yes | |
| Plocks/flocks, sessions and heartbeat | yes, but not needed | #101. The heartbeat `os.Exit` is at base.go:919-927 |
| Symlink | wired (namespace.go:272) | TM does not need it |
| Hard links, quotas, ACL, dir stats, multi-client sessions, FUSE/vfs control file, access log, metrics, writeback, sharding, tiers, file/mem/sftp/redis back ends | **no** | Carried along. 781 unreachable functions in the vendored code (research-code-quality.md §6) |

**Size.** Non-test lines (test lines in brackets): juicefs 35,417 [1,045] (meta 17,783, vfs 5,529, chunk 4,315, object 3,842, utils 1,658, fs 1,640, acl 404, compress 125, version 121), xorm 19,703, mpb 3,218. That is **58.3k vendored lines that exist only for storage**, against 3.8k lines of our own code. JuiceFS-specific glue in our code is about 1.2k lines: storage 445, backup ~690 (most of it), logging/native.go 59.

**Dependencies (measured).** 63 modules are linked into the binary (`go version -m`). **37 of them are reachable only through juicefs, xorm or mpb, or through the logrus bridge that exists for JuiceFS.** They include redis, sftp, goleveldb, snappy, gmsm, prometheus (4 modules), protobuf, DataDog/zstd (cgo), go-lz4, murmur3, msgpack, go-json, groupcache, gspt, dnscache, ratelimit, ewma, pkg/errors, go-humanize, fastwalk, go-isatty, go-runewidth, xorm.io/builder, x/sync and logrus. 9 more go.mod entries are JuiceFS test or indirect deps (testify, ginkgo, gomega, fsnotify, x/net, klauspost x2, spew, difflib). So **go.mod would go from 75 requires to about 29**: the aws-sdk-v2 tree, smithy, mattn/go-sqlite3, uuid, yaml.v3, x/crypto, x/sys, x/exp, ber and the test SMB client.

**Binary size (measured, linux/arm64, go1.26.3).** I built a stub that links smb2/server, config, logging, aws s3/config, database/sql with mattn sqlite, and crypto/aes-gcm. It is 18.5 MB (12.0 MB stripped). Today's binary is 42.8 MB (28.2 MB stripped). Adding about 3k lines of engine would bring the binary to roughly 19-20 MB, **about 55% smaller**. One of the two cgo deps goes away (zstd). mattn sqlite stays.

## 2. Time Machine's write pattern on a network sparsebundle

Sources: the repo, PR #63 / issue #58, the #64 reports, research-macos-client.md, and my own knowledge (marked *K*, medium confidence).

- **Layout.** `<host>.sparsebundle/` contains Info.plist, Info.bckup, token, `bands/<hex>` and *K* a `mapped/` dir, a lock file and a few `com.apple.TimeMachine.*.plist`. Since macOS 11 the image is APFS with snapshots inside, not HFS+ with hard links (*K*). On the share TM needs regular files and directories only. It does not use hard links, symlinks or special files (research-smb.md:40).
- **Band size depends on the share size we report.** The observed values fit size/4096 with an 8 GiB cap:
  - 1.13 PB reported → 8.59 GB bands;
  - Apple smbd at 348 GB → 85.1 MB;
  - our 1 TiB clamp (smbfs/attributes.go:146-160, PR #63) → 268.4 MB.

  Formatting a 1 PiB-sized image wrote about 19 GB in about 290 s, against 197 MB on Apple's smbd (PR #63). The first small backup now stores 1.3-1.8 GB (README.md:110). #58 (open) proposes a configurable capacity. With a native engine this is a constant in StatFS.
- **File count.** Bands = used bytes / band size, so about 4 per GB at 268 MB bands. A 1 TB history is about 4,000 band files plus about 10 small files, in two directory levels. The namespace is tiny, so metadata is dominated by the block map.
- **Write shape.** WRITE requests are 1 MiB / 512 KiB / 256 KiB (46/46/8% by bytes, workload-match.md known-numeric-summary, rc.7 run 37120878886), using multi-credit LARGE_MTU. APFS is copy-on-write, so new data goes to free extents. Within a band that is mostly sequential runs, but at **random offsets inside existing band files**. APFS metadata (checkpoints, spaceman, omap) is **overwritten in place** in hot, low-numbered bands (*K*). After TM deletes old snapshots, freed extents are reused, which means overwrites of cold data. Band files are sparse: holes must read as zeros and must not cost storage.
- **Reads happen during backup** (APFS metadata, the omap, verification; *K*). With JuiceFS, a read of a chunk with ≥5 slices triggers a 64 MiB compaction rewrite (base.go:2133).
- **Durability.** We advertise `kAAPL_SUPPORTS_FULL_SYNC`, so F_FULLFSYNC becomes SMB FLUSH (research-macos-client.md §3). DiskImages sends it at APFS barriers, on each dirty band handle. *K*: every few seconds to tens of seconds while writing, plus at snapshot and end of backup. **FLUSH is the only durability promise that matters.** It applies to the whole file across all handles (#89).
- **Deletes, truncates, renames.** Bands are deleted or shrunk when the image is compacted or unmapped (*K*, frequency unknown). A whole bundle is deleted after a failed first backup. Renames are rare: safe-save of small plists (*K*).
- **Xattrs and streams** are few and under 64 KiB: AFP_AfpInfo (the bundle bit) and some `com.apple.*` values (research-smb.md:32).
- **Clients and locks.** One client and one writer. In the rewrite, lock and share-mode state lives in SMB-layer memory, not in storage (research-smb-rewrite.md §2).

## 3. Minimal engine design (TM-only, single writer)

The engine *is* the ~17-method FS interface from research-smb-rewrite.md §2. There is no separate adapter layer.

**S3 layout** (prefix `s3-smb/`):
- `volume.json`: UUID, block size, format version.
- `key.json`: a 32-byte data key, wrapped with scrypt(passphrase) and AES-GCM.
- `data/<128-bit random id>`: one immutable object per block version.
- `meta/<UTC ts>-<rand>.db.gz`: metadata snapshots, encrypted.

Every PUT uses `If-None-Match: *` (the existing requirement, README.md:112). **Ids are random and never reused**, so no counter has to survive recovery. That removes the #91 and #92 class by construction, and cache entries keyed by id can never be stale.

**Blocks.** Each file is a sequence of fixed 4 MiB blocks (make it a volume constant; 1 MiB is the alternative to measure). The block map holds `(ino, idx) → (object id, len)`. A missing row is a hole and reads as zeros. An all-zero block is stored as a hole, which gives most of the benefit of zstd on zeros with no dependency. An overwrite writes a new object for the whole block (read-modify-write from the dirty buffer, then the cache, then S3). There are no slices and no compaction.

**Local metadata.** One SQLite file through `database/sql` and mattn/go-sqlite3 (already a dependency and maintained), WAL with `synchronous=FULL`, hand-written SQL, no ORM.

```
volume(k TEXT PRIMARY KEY, v)                        -- uuid, block size, schema version
inodes(ino INTEGER PRIMARY KEY, type, mode, size, mtime_ns, ctime_ns, btime_ns)
dirents(parent, name, ino, PRIMARY KEY(parent,name))  -- no hard links: PathOf walks dirents by ino (index on ino)
xattrs(ino, name, value BLOB, PRIMARY KEY(ino,name))  -- also named streams, 64 KiB cap
blocks(ino, idx, obj BLOB(16), len, PRIMARY KEY(ino,idx))
```

**Write path and FLUSH.**
- Each inode has one in-memory state object, shared by every handle: dirty blocks and the logical size. GetAttr, Lookup and ReadDir read the size from it, so #59, #84, #96 and #113 cannot exist.
- WRITE copies into dirty blocks and is acknowledged at once, which is disk semantics.
- Under memory pressure, past a global cap such as 256 MiB, dirty blocks may be **uploaded** early but are **not committed**.
- FLUSH or CLOSE of an inode, on any handle (#89):
  1. Upload all of the inode's dirty and early-uploaded-but-uncommitted blocks in parallel.
  2. Commit **one SQLite transaction**: the block rows, size and mtime.
  3. Reply.
- Namespace, attribute and xattr changes each commit at once in their own transaction.
- Truncate drops dirty blocks past the new end and deletes `blocks` rows in the same transaction.

**Read path.** Look in the dirty block, then the disk cache (file named by object id, length checked, GCM authenticates on load), then S3 GET with singleflight. Read ahead one block when access is sequential. The cache is advisory and can be wiped at any time.

**Crash rules (the whole spec):**
1. A PUT succeeds before any row references the object.
2. Objects are immutable, keys are unique, and nothing is ever overwritten.
3. One FLUSH is one transaction.
4. GC deletes only objects that the current DB and every retained snapshot leave unreferenced, and that are older than a grace period.
5. A snapshot is uploaded and read back before older snapshots are pruned.

After a crash, **each file equals its content at its last successful FLUSH, and every acknowledged namespace operation is present.** This one invariant is easy to test.

**GC (mark and sweep, no trash tables).**
- Mark: the object ids in the current DB plus every retained snapshot.
- Sweep: LIST `data/` and delete ids that are unreferenced and whose LastModified is older than the grace period (for example 2× the backup interval). The grace period covers in-flight uploads.
- Run it daily, and only while the newest snapshot is fresh. That keeps today's `Protection` idea (backup/protection.go) but moves it to a single gate.
- It also cleans crash orphans and needs no state carried across recovery, so the #114 class is gone.
- **Retention policy change:** keep snapshots only inside the window you are willing to pay garbage for (for example hourly for 2 days, daily for 14 days). Every retained recovery point is then fully restorable. Today the README keeps monthly dumps for 2 years that may point at deleted blocks (docs/recovery.md "Why retention matters").

**Metadata backup and recovery.**
- Backup: `VACUUM INTO` a temp file (a consistent online copy), gzip with the stdlib, AES-GCM, PutIfAbsent, then read back and compare the hash. That replaces BackupTo, the JSON dump and LoadMeta.
- When to back up: on a schedule (1 h), and also after a write burst goes quiet (for example 5 min with no FLUSH), so the end of each TM backup is protected quickly.
- Recovery on a fresh machine: fetch the newest snapshot, decrypt, gunzip, `PRAGMA integrity_check`, check the schema and volume UUID, rename it into place, start with an empty cache.

**Encryption and compression.**
- AES-256-GCM from the stdlib with a 96-bit random nonce per object (well under 2^32 objects) and AAD = object key, so objects cannot be swapped. Overhead is 28 bytes per object. The key is wrapped by scrypt (x/crypto, already a dependency).
- No compression at first: TM network backups are often encrypted by the client (the APFS-in-image data is then ciphertext), and zero elision covers the formatting zeros. If measurements show it is worth it, add stdlib flate behind a format flag.

**Single writer.** Keep the state lock. Optionally add an S3 lease object updated with conditional PUT, so that a second host refuses to start.

**Estimate.**

| Part | Non-test lines |
|---|---|
| S3 client wrapper (put-if-absent, get-range, list, delete, retries and timeouts via ctx) | 300 |
| Crypto (key wrap, seal/open) | 150 |
| Metadata store (schema, ~25 queries, transactions) | 700 |
| Inode state, dirty buffers, read-modify-write, flush, upload pool, backpressure | 700 |
| Disk cache (LRU by bytes, singleflight, readahead) | 350 |
| Snapshot backup, restore, retention | 300 |
| GC | 200 |
| FS-interface surface (path rules, streams as xattrs, StatFS) | 400 |

That is **about 3.1k non-test lines** (it replaces storage 445, most of backup 690, and most of the planned 1.2k-line adapter). Tests: **about 4-5k lines**.

## 4. Gains and losses

**Gains**
- Minus 58k vendored lines and minus ~46 go.mod entries. The binary goes from 43 MB to about 20 MB.
- Every line is ours, so `forbidigo panic` and the strict #141 config reach zero for the whole repo. Today the choice is to exclude juicefs and xorm from lint (815 juicefs findings plus most of the 423 thirdparty ones) or to fork them deeper.
- The 44 JuiceFS panic, Fatal and Exit sites, including the background `os.Exit`, are gone, and there is no xorm (research-panics.md §1).
- Semantics fit SMB: one state per inode, FLUSH per file, size in memory. Today's model is one writer per inode but "length known only at flush", with per-handle `wdata`, and needs workarounds (attributes.go:39-54).
- No slices and no compaction, so less write amplification and steadier bucket growth (README.md:107-111). Holes cost nothing.
- Recovery is a file copy, not a JSON rebuild.

**Where the current bugs live**

| Bug | Location | With a new engine |
|---|---|---|
| #110 (WithTimeout race) | JuiceFS code | gone |
| #128, #144 (disk cache races) | JuiceFS code | gone |
| #131 (zstd overrun) | JuiceFS code | gone |
| #92, #114 (dump/load) | JuiceFS code | gone by design (random ids, snapshot copy, mark-and-sweep) |
| #91 | JuiceFS counter rebuild plus our reuse of the cache dir | gone by design |
| #59, #84, #89, #96, #113 | Integration: the SMB "one file" model against JuiceFS per-handle writers | gone by design. The rewrite's adapter can also fix them, but needs more vfs patches (e.g. exposing the writer's in-memory length) |
| #101 | JuiceFS plocks we should not use | fixed by the SMB rewrite either way |
| #102 | Our shutdown order plus the delete hook in `maintenanceTxn` | easier: unlink becomes a metadata op and only GC deletes objects |

So 7 of 14 are inside JuiceFS code, 5 are mismatches between JuiceFS and SMB semantics, and 2 are ours or our use of it. **Both critical silent-corruption bugs (#91, #92) and #131 come from paths most JuiceFS deployments don't use**: JSON dump/load as the recovery mechanism, and the embedded `fs` API over SQLite via xorm instead of FUSE over Redis/MySQL/TiKV. The "production maturity" argument is weakest exactly there.

**Losses and risks**
- **Maturity.** JuiceFS has years of large-scale production use. Its writer, cache and retries have absorbed edge cases a new engine will meet again: partial reads, throttling, lost PUT responses, a full cache disk, slow S3. A new engine *will* have data-loss bugs at first. The question is whether tests find them before users do.
- **Performance unknowns.** A partial write into a cold 4 MiB block needs a GET first (read-modify-write). TM's copy-on-write pattern should make that mostly a cache hit on hot metadata, but it is not measured.
- **Features dropped:** compression, user-visible trash, symlinks, hard links, quotas, multi-client, metrics, writeback. None is needed for TM.
- **Work thrown away:** the JuiceFS patch set (docs/vendored.md) and its tests. What survives: test/e2e (black box, 2.1k lines), the Mac harness, the app/config/logging tests, the smbfs test scenarios, and the Protection, receipt and PutIfAbsent ideas.
- **Effort:** about 3k + 4-5k lines plus Mac validation, comparable to M3 of the SMB rewrite.

**What would make it trustworthy** (gate the flip on all of these):
1. **Model-based tests.** Random operation sequences (create, write at random offset and length, truncate, flush, close, rename, unlink, xattr, restart, recover-from-snapshot, GC) run against an in-memory reference with "durable" and "volatile" views, with every read compared. The same suite runs against JuiceFS through the interface (differential) and against the SMB layer through the model.
2. **Crash-point injection.** A hook at every step (before and after PUT, before and after commit, during VACUUM INTO and upload, between GC LIST and DELETE). Enumerate the crash points deterministically for each generated sequence, reopen from the on-disk DB and the fake S3, and assert the crash invariant and "every referenced object exists".
3. **A flaky S3 fake.** 5xx and throttling bursts, timeouts, slow and truncated bodies, PUTs that succeed but lose their response, short LIST pages. Assert that no FLUSH succeeds unless the data is durable, and that a read returns either the right bytes or an error, never wrong bytes. Then the #142 chaos suite on top, with MinIO.
4. **Recovery tests.** Fresh machine, no cache. Stale cache plus recovery (the #91 scenario). Recover, write, read. Recover twice. GC after recovery keeps everything that retained snapshots reference.
5. Fuzz the object envelope and snapshot decoders. `-race` everywhere.
6. Real TM: two green Mac acceptance runs including kill/restart/machine-loss, plus a multi-day soak with incremental backups and snapshot thinning.

## 5. Ordering

- **Parallel build is feasible and fits the SMB plan.** M0 defines the FS interface with no JuiceFS types and `syscall.Errno` errors. Write three things against it:
  - (a) an in-memory reference model (~500 lines; the SMB M1-M3 work develops against it);
  - (b) the new engine;
  - (c) the JuiceFS-backed adapter that M1(e) already plans.
- One conformance, model and crash suite runs against all three. Keep the build-tag switch from research-smb-rewrite.md §6, with a fresh bucket per engine and no migration.
- Flip when (b) passes everything (c) passes, plus the crash and flaky-S3 suites, plus the Mac runs.
- **Caveat:** to pass conformance, (c) must fix #59/#84/#89/#96/#113 inside JuiceFS's writer model, which is work that later gets deleted. So treat (c) as a fallback only. The old stack (smb2 + old smbfs + JuiceFS) stays the shipped one until new SMB + new engine pass together. Flipping two components at once is less risky because both are tested against the shared model first.
- **"Keep JuiceFS and fix only the ~10 panic sites" is the cheap option**: about 1-2 weeks including #91/#92/#128/#131/#144, whose fixes mostly exist on swarm branches (research-panics.md §6, research-storage-tests.md §5). It is safer now, but it does not meet "small, clean, fully linted", and it keeps the dump/load and compaction model behind the worst bugs.

## 6. Recommendation

**Replace JuiceFS with a ~3k-line TM-only engine, built behind the rewrite's FS interface in parallel, and flip only after the model, crash-injection, flaky-S3, recovery and Mac gates pass. Confidence: medium (about 65%).**

Reasons:
- The owner's goals (small, panic-free, fully linted, no compatibility constraints) cannot be met while keeping 58k vendored lines.
- TM's workload is narrow: one writer, a few thousand large sparse files, and per-file FLUSH as the only durability point.
- The bugs found so far sit in exactly the parts of JuiceFS that are least mature in our configuration and least matched to SMB.
- Random immutable object ids, one transaction per FLUSH and mark-and-sweep GC make the critical bug classes impossible rather than patched.

**What would flip it to "keep JuiceFS":**
- the owner needs a reliable release within weeks rather than months;
- or no one will build the crash and simulation test harness. Without it, a new engine is the riskier choice.

**In the meantime:** if anything ships before the flip, merge the small JuiceFS fixes for #91/#92 (one PR), #128/#131 (page-pool-v3) and #144 now.
