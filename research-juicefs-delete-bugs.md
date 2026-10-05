# Research: JuiceFS data loss cases from late or wrong deletes

Ticket: [#599](https://github.com/djosh34/s3-smb/issues/599). Map: [#572](https://github.com/djosh34/s3-smb/issues/572). Context: [#595](https://github.com/djosh34/s3-smb/issues/595), [#597](https://github.com/djosh34/s3-smb/issues/597), [research-juicefs-deletes.md](https://github.com/djosh34/s3-smb/blob/research/juicefs-deletes/research-juicefs-deletes.md) ([#596](https://github.com/djosh34/s3-smb/issues/596)).

## Labels

- **Documented**: stated in a JuiceFS issue, PR description, maintainer comment, discussion answer, release note or doc.
- **Source**: read in a PR diff, in upstream JuiceFS `main` (`adcca1cc`, 2026-09-29), or in the vendored copy here (`internal/juicefs`, v1.4.1).
- **Inferred**: my reasoning. Not tested.

"Production" means a user reported it from a real system. "Review" means it was found by reading code, a test, CI or a fuzzer. When the issue does not say, I write "unknown".

## Short answer

- I found **15 real cases** where JuiceFS deleted or lost data that was still needed, or would have. Only **3 were seen in production** with lost data: a `gc` sweep ([#6479](https://github.com/juicedata/juicefs/issues/6479)), a file created in a directory already in the trash ([#3737](https://github.com/juicedata/juicefs/issues/3737)), and duplicate PUTs of the same key on a flaky store ([#3260](https://github.com/juicedata/juicefs/issues/3260)). The rest were found by review, tests or CI.
- **The biggest class is GC and leak sweeps: 5 cases.** Four of them are one pattern: "list everything that is live, delete everything else", where the list was silently incomplete.
- **Most cases come from a clever cleanup: 9 of 15.** That means reference counts, "not in the list means garbage", or "delete my upload if nobody needs it". Three come from a blunt rule set up wrong: a wait measured from the wrong moment, a broken timestamp, or the wrong clock. **No case was caused by the plain fixed wait itself.** Twice the fixed wait was added *to prevent* loss.
- **I found no case of a late DELETE killing a reused name**, and no case caused by S3 LIST lag. JuiceFS's maintainers confirm that names collide after a metadata restore ([discussion #7359](https://github.com/juicedata/juicefs/discussions/7359)), but nobody reported losing data that way.
- **Our design avoids most of them by construction.** Random names never reused, one writer, SQLite snapshots and no automatic sweep of unknown chunks remove 10 of the 15. The 5 that partly apply are side effects of a retried transaction (2), a trash wait computed wrong, a stale second server, and duplicate PUTs of one key. A future manual cleanup command would bring back the sweep class.
- **One rule from JuiceFS is worth copying word for word.** A maintainer refused to delete an upload after a failed metadata commit: "It's possible that the `Write` actually succeeded, in which case deleting the objects will corrupt the file", and "the consequences of data loss are often far more severe than those of data leakage" ([PR #4808](https://github.com/juicedata/juicefs/pull/4808)).

## Summary table

"Ours" is whether our design (random chunk names never reused, SQLite plus copies, fixed-wait trash, one writer, no automatic sweep of unknown chunks, per [#597](https://github.com/djosh34/s3-smb/issues/597) option X) is exposed. All "Ours" entries are Inferred.

| # | Case | Class | Found | Clever or blunt | Fixed in | Ours |
|---|---|---|---|---|---|---|
| 1 | [#6479](https://github.com/juicedata/juicefs/issues/6479) `gc --delete` deleted many live objects, slice list was partial | GC sweep | Production | Clever | v1.3.1 | Not exposed, no sweep. Exposed if a manual sweep is added |
| 2 | [#7530](https://github.com/juicedata/juicefs/issues/7530) `gc` read a partial `jfs_chunk` table as success after a dropped DB connection | GC sweep | Review, reproduced on MySQL | Clever | `main` only | Same as 1 |
| 3 | [PR #5068](https://github.com/juicedata/juicefs/pull/5068) `gc` took its 1 hour cutoff after listing slices, so on large volumes it deleted new live objects | GC sweep | Unknown | Blunt, wrong anchor | v1.1.4, v1.2.1 | Same as 1 |
| 4 | [PR #1110](https://github.com/juicedata/juicefs/pull/1110) `gc` inode sweep deleted an inode renamed during a non-atomic scan | GC sweep (metadata) | Review | Clever | v1.0.0-beta1 | Not exposed |
| 5 | [PR #7449](https://github.com/juicedata/juicefs/pull/7449) two GC runs both lowered the count of one delayed slice, deleting a live slice | GC sweep, ref counts | Review | Clever | `main` only | Not exposed, one writer |
| 6 | [PR #6697](https://github.com/juicedata/juicefs/pull/6697) duplicate upload of a staged block, then "not needed" cleanup deleted the valid object | Client upload cleanup | Unknown | Clever | v1.3.3, v1.4.0 | Not exposed |
| 7 | [PR #7460](https://github.com/juicedata/juicefs/pull/7460) batch unlink kept a "delete this inode" list from a rolled-back attempt | Txn retry | Review | Clever | `main` only | Partly exposed |
| 8 | [PR #7477](https://github.com/juicedata/juicefs/pull/7477) "skip trash" leaked from a failed attempt into the retry, file deleted for good | Txn retry, trash | Review | Clever | `main` only | Partly exposed |
| 9 | [PR #7526](https://github.com/juicedata/juicefs/pull/7526) same name twice in one batch unlink, link count went to 0, hard-linked file deleted | Other, link counts | Review | Clever | `main` only | Not exposed |
| 10 | [PR #6564](https://github.com/juicedata/juicefs/pull/6564) batch unlink missed the "still open" flag, open file could be deleted | Other, open files | Review | Clever | Before any release | Not exposed |
| 11 | [#3737](https://github.com/juicedata/juicefs/issues/3737), [#2774](https://github.com/juicedata/juicefs/issues/2774) a file created in a directory already moved to trash, later deleted with the trash | Trash timing, stale names | Production | Neither, namespace race | v1.1.0 | Not exposed |
| 12 | [#2053](https://github.com/juicedata/juicefs/issues/2053) TKV delayed-slice key had ID and timestamp swapped, slices cleaned "at a wrong time" | Trash timing | Review | Blunt, broken timestamp | v1.0.0-rc1 | Exposed to the class |
| 13 | [#7431 SQL-C04](https://github.com/juicedata/juicefs/issues/7431) stale-session cleanup has no fencing, can delete an open-unlinked file of a client that came back | Late request, stale client | Review | Blunt timeout | Open | Partly exposed |
| 14 | [#6217](https://github.com/juicedata/juicefs/pull/6217) TiKV metadata GC used the client clock, a fast clock could drop versions still in use | Other, clock | Review | Blunt, clock | v1.4.0 | Not exposed with rule 5 |
| 15 | [#3260](https://github.com/juicedata/juicefs/issues/3260) writeback re-uploaded the same key many times at once, objects left unreadable | Client-side retries | Production | Neither | `main` in 2023 ([#3157](https://github.com/juicedata/juicefs/pull/3157)) | Partly exposed |

Related, but not data loss (section 4): compaction deleting slices a reader still uses, which gives a temporary EIO ([#4012](https://github.com/juicedata/juicefs/issues/4012), [#6000](https://github.com/juicedata/juicefs/issues/6000), [#2562](https://github.com/juicedata/juicefs/issues/2562)). Also name collisions after a metadata restore ([discussion #7359](https://github.com/juicedata/juicefs/discussions/7359)), backups that are not a consistent snapshot ([#7431 SQL-H02](https://github.com/juicedata/juicefs/issues/7431)), and late PUTs that leak objects ([PR #4748](https://github.com/juicedata/juicefs/pull/4748)).

### Count per cause class

| Class | Cases | Production | Numbers |
|---|---:|---:|---|
| GC or leak sweep deleting live data | 5 | 1 | 1, 2, 3, 4, 5 |
| Transaction retry leaking delete state | 2 | 0 | 7, 8 |
| Other bookkeeping (link counts, open files, clock) | 3 | 0 | 9, 10, 14 |
| Trash or delayed-delete timing | 2 | 1 | 11, 12 |
| Client cleanup of its own upload | 1 | 0 | 6 |
| Late request, stale client | 1 | 0 | 13 |
| Client-side retries | 1 | 1 | 15 |
| Slice or ID reuse | 0 | 0 | hazard confirmed, no incident (4.2) |
| Metadata backup or restore | 0 | 0 | two open hazards (4.2) |
| Compaction | 0 | 0 | 3 cases of temporary EIO (4.1) |
| Object storage consistency, LIST lag | 0 | 0 | none found |

Some cases fit two classes. I counted each once, under its main cause.

## 1. GC and leak sweeps

### 1. `gc --delete` deleted live objects on TiKV ([#6479](https://github.com/juicedata/juicefs/issues/6479))

- **What happened.** A user ran `juicefs gc --delete` on a large volume and saw "a massive number of objects being deleted even though they are still referenced by files. This caused data loss" (Documented). The reporter suspected that `ListSlices` "just ignores all errors", so a transient error gave a partial list that looked complete (Documented). The maintainer agreed: "This error handling could indeed lead to mistaken deletion" (Documented).
- **Class.** GC sweep. **Found:** production. The reporter could not reproduce it.
- **Fix.** [PR #6481](https://github.com/juicedata/juicefs/pull/6481) returns the scan error. The bug came from [PR #5080](https://github.com/juicedata/juicefs/pull/5080), which made the TiKV scan a stream (Documented in the issue).
- **Version, date.** Reported 2025-11-20 on `main`. Fix in v1.3.1 (2025-12-02, release note "fix error handling in ListSlices") (Documented). #5080 merged 2025-04-15, so v1.3.0 was affected (Inferred from dates).

### 2. `gc` treated a cut-off SQL read as complete ([#7530](https://github.com/juicedata/juicefs/issues/7530))

- **What happened.** `gc` reads all of `jfs_chunk` to build the live set. MySQL dropped the connection after 10 minutes and 120 million rows. The vendored XORM returned `nil` from `Find` because it never checked `rows.Err()` (Documented, with a reproduction against a real MySQL). Any object missing from the partial set would count as leaked and be deleted with `--delete` (Documented).
- **Class.** GC sweep. **Found:** review, reproduced the truncated read. No deletion was reported.
- **Fix.** The XORM upgrade in [PR #7411](https://github.com/juicedata/juicefs/pull/7411) returns `rows.Err()` (Documented in a comment). The maintainer closed the issue as fixed on `main` (Documented).
- **Version, date.** 2026-09-09. #7411 merged 2026-08-18, after v1.4.1 (2026-07-30) and v1.3.3 (2026-08-07). So no release has the fix yet (Inferred from dates). The prior report found the same XORM fork vendored here at `internal/thirdparty/xorm`.

### 3. `gc` measured its 1 hour skip from the wrong moment ([PR #5068](https://github.com/juicedata/juicefs/pull/5068))

- **What happened.** `gc` skips objects younger than 1 hour, so that uploads not yet committed are not taken for leaks. The cutoff `now - 1h` was computed *after* listing all live slices (Source: the diff moves `maxMtime := time.Now().Add(time.Hour * -1)` from after the slice scan to the start). On a large volume the scan takes more than an hour. An object uploaded and committed during the scan is then missing from the live set, but older than the cutoff, so it is deleted (Inferred from the diff). The release note says "fix the issue that objects may be deleted by mistake in large systems" (Documented).
- **Class.** GC sweep. A blunt fixed wait, anchored at the wrong time.
- **Found:** unknown. The PR has no description and no linked issue.
- **Fix.** Take the cutoff before the scan. Upstream `main` still does this (Source: `cmd/gc.go:122` before `ScanSlices` at `:240`).
- **Version, date.** Merged 2024-08-09. Fix in v1.1.4 and v1.2.1 (2024-09-02) (Documented).

### 4. `gc` inode sweep deleted a renamed inode ([PR #1110](https://github.com/juicedata/juicefs/pull/1110))

- **What happened.** `gc` scanned all directory entries, then deleted inodes that no entry pointed to. "The scanning of entries is not atomic", so an inode renamed during the scan could be missed under both names and "be identify as dangling and deleted" (Documented). The 1 hour guard used `atime`, which a rename does not change (Source: diff, `Atime` to `Ctime`).
- **Class.** GC sweep, on metadata. **Found:** review.
- **Fix.** Use `ctime`, which a rename does update.
- **Version, date.** 2021-12-09, in v1.0.0-beta1 ("Fix potential metadata corrupt in Redis caused by gc (#1110)") (Documented).

### 5. Two GC runs on the same delayed slice ([PR #7449](https://github.com/juicedata/juicefs/pull/7449), [#7431 SQL-C03](https://github.com/juicedata/juicefs/issues/7431))

- **What happened.** A slice has one live reference and one delayed (trash) reference, so `refs = 2`. Two GC processes read the same delayed row. GC1 lowers refs to 1 and deletes the row. GC2 waits for the row lock, lowers refs to 0, deletes 0 rows, does not check, and commits. The live slice is now deleted (Documented). "Two maintenance jobs, repeated scheduling or two management components running GC at the same time is enough to trigger it" (Documented, translated).
- **Class.** GC, reference counts. **Found:** review (a concurrency audit of the SQL engine).
- **Fix.** Claim the delayed row first, and only the claimer lowers the count.
- **Version, date.** Merged 2026-08-26 on `main`. The audit lists `main`, `release-1.4` and `release-1.3` as affected, and says the fix is not confirmed in the stable branches (Documented). The vendored v1.4.1 here does not check the delete result in `doCleanupDelayedSlices` (Source: `internal/juicefs/pkg/meta/sql.go:3841`).

### What the sweep cases share

- Cases 1, 2 and 4 are one pattern: build a set of live things, then treat everything outside it as garbage. An incomplete set looks exactly like a complete one, and the error is silent (Inferred).
- Case 3 is the guard for that pattern, measured from the wrong moment.
- Case 5 is a reference count lowered twice. A count does not know *who* is lowering it.
- JuiceFS's docs still call leaks "rare" and say a leak "without any special file system manipulation ... could well indicate a bug" (Documented: `docs/en/reference/command_reference.mdx:338`). The sweep that cleans those rare leaks caused its worst reported data loss (Inferred).

## 2. Deletes in the normal file system path

### 6. Duplicate upload, then "not needed" cleanup deleted the valid object ([PR #6697](https://github.com/juicedata/juicefs/pull/6697), [PR #4748](https://github.com/juicedata/juicefs/pull/4748))

- **Background.** In writeback mode a block is staged on local disk and uploaded later. If the slice was deleted before the upload ran, the upload leaked an object. "We observed several cases of leaked objects when `write-back` is enabled and `trash-days` set to 0" (Documented, PR #4748). The fix: after a successful upload, if the key is no longer pending, delete the object again (Source: #4748 diff, `isPendingValid`).
- **What went wrong.** A race on a plain `uploading` bool let two workers upload the same staged block (Source: #6697 diff, `bool` to `atomic.Bool` with `CompareAndSwap`). PR #6697 says "ref #4748 may delete object if conflict" (Documented). The v1.3.3 release note: "cache: prevent duplicate staged-block uploads from deleting valid objects (#6697)" (Documented). The likely order: the first upload finishes and removes the pending key, the second finishes, sees the key is not pending, and deletes the object that is now live (Inferred from both diffs).
- **Class.** Client cleanup of its own upload. A clever cleanup.
- **Found:** unknown.
- **Version, date.** Bug added in v1.2.0 (2024-06-19). Fixed in v1.4.0-beta1 and v1.3.3 (2026-08-07) (Documented). Writeback mode only.

### 7. Batch unlink kept deletes from a rolled-back attempt ([PR #7460](https://github.com/juicedata/juicefs/pull/7460), [#7431 SQL-C02](https://github.com/juicedata/juicefs/issues/7431))

- **What happened.** The first transaction attempt computes `nlink = 0` for inode 100 and adds it to a Go map `delNodes`. The SQL fails with a retryable error and rolls back. A concurrent change keeps inode 100 alive. The retry commits without deleting it. But the map still holds inode 100, and the background job deletes its data blocks (Documented). "A rollback only rolls back the database, not changes to an outside Go map" (Documented, translated).
- **Class.** Transaction retry. **Found:** review.
- **Fix.** Per-attempt state, published only after the commit.
- **Version, date.** 2026-08-26 on `main`. Affects `main` and `release-1.4` (Documented).

### 8. "Skip trash" leaked into the retry ([PR #7477](https://github.com/juicedata/juicefs/pull/7477))

- **What happened.** `doUnlink`, `doRmdir` and `doRename` decide the trash target before the transaction. A repair path in one attempt can set `trash = 0`. The value leaks into the next attempt, which "can then permanently delete the entry instead of moving it to trash" (Documented). A deterministic SQLite test reproduces it (Documented).
- **Class.** Transaction retry, trash. **Found:** review.
- **Fix.** Reset `trash = requestedTrash` at the start of every attempt.
- **Version, date.** Merged 2026-09-09 on `main`. The vendored v1.4.1 does not have `requestedTrash` (Source).

### 9. Same name twice in one batch unlink ([PR #7526](https://github.com/juicedata/juicefs/pull/7526))

- **What happened.** A name repeated in one `BatchUnlink` call lowered the shared `Nlink` twice. A file with two hard links went from 2 to 0, was queued for deletion and deleted "while the other hard link still referenced it, a dangling entry plus data loss" (Documented). All three engine families were affected (Documented).
- **Class.** Other, link counting. **Found:** review ("unexpected inputs from SDK usage").
- **Version, date.** Merged 2026-09-09 on `main`.

### 10. Batch unlink missed an open file ([PR #6564](https://github.com/juicedata/juicefs/pull/6564))

- **What happened.** When several hard links of one inode were removed in one batch, the "still open" flag could be read from the wrong entry. "An open file might not be added to the sustained set, potentially causing data loss if the file is deleted while still open" (Documented).
- **Class.** Other. **Found:** review.
- **Version, date.** Merged 2025-12-29. SQL batch unlink was merged 2025-12-19 and first released in v1.4.0-beta1 (2026-04-29), so no release had this bug (Inferred from dates).

### 11. A file created in a directory already in the trash ([#3737](https://github.com/juicedata/juicefs/issues/3737), [#2774](https://github.com/juicedata/juicefs/issues/2774), [PR #3864](https://github.com/juicedata/juicefs/pull/3864))

- **What happened.** Client A removes a directory, which moves it to the trash. Client B still has the directory in its entry cache and creates a file in it. The create succeeds (Documented, #2774). "The creation succeed but user can't access the file from given path. The file will be eventually removed from trash" (Documented, PR #3864). In #3737 a Spark job reported success, but the output file was missing, "nor does it exist in metadata or Trash" (Documented). A maintainer linked #3737 to #2774 (Documented).
- **Class.** Trash timing, with a stale name. **Found:** production (#3737), review (#2774).
- **Fix.** Refuse to create entries in a trash directory, and retry the lookup.
- **Version, date.** #2774 on v1.0.0 (2022-09). Fixed in v1.1.0 (2023-09-04) (Documented).
- **Similar, smaller.** [PR #5414](https://github.com/juicedata/juicefs/pull/5414): an unlink at 10:59:59 picks trash directory `10`, the hourly cleanup removes `10`, and the file is moved under a deleted directory and "becomes dangling" (Documented). Fixed in v1.3.0. This loses the trash copy, not live data (Inferred), so I do not count it.
- **Similar, not a bug per maintainer.** [#6597](https://github.com/juicedata/juicefs/issues/6597): `open(O_CREAT)` in a directory renamed by another client puts the file in the trash. The maintainer says "There won't be any data loss" (Documented). Not counted.

### 12. Delayed slices cleaned "at a wrong time" ([#2053](https://github.com/juicedata/juicefs/issues/2053))

- **What happened.** In the TKV engine the delayed-slice key was written as `L + chunkid + ts` but parsed as `L + ts + chunkid` (Documented). The maintainer: "only the timestamp will be wrongly parsed, leading to some slices cleaned at a wrong time" (Documented). These slices only matter for restoring an older metadata backup ([#1521](https://github.com/juicedata/juicefs/issues/1521), section 5). So the risk was a broken restore, not loss of live data (Inferred). If the parsed time was the small slice ID, the slices looked very old and were deleted at once (Inferred, not checked).
- **Class.** Trash timing. A blunt wait, with a broken timestamp. **Found:** review.
- **Version, date.** The delayed slices came in v1.0.0-beta3 (2022-05-05). Fixed 2022-05-19, before v1.0.0-rc1 (Documented: release notes).

### 13. Stale-session cleanup without fencing ([#7431 SQL-C04](https://github.com/juicedata/juicefs/issues/7431), [#7462](https://github.com/juicedata/juicefs/issues/7462), [discussion #7461](https://github.com/juicedata/juicefs/discussions/7461))

- **What happened.** A client is paused, so its session looks stale. A cleaner deletes its locks, its "sustained" (open but unlinked) inodes, and its session row. The client resumes and recreates the same session ID. "Open-unlinked files may be deleted as stale sustained inodes" (Documented, translated). #7462 shows the lock side. A session is stale after about 60 s, while the watchdog kills a frozen mount after about 120 s. In between, the old client can still write while another client holds the lock (Documented).
- **Maintainer view.** "Agreed the race is real", but failing on a stall "would turn a rare correctness issue into a common availability one" (Documented, discussion #7461). That is a design choice, not a bug to fix.
- **Class.** Late request, from a stale client. **Found:** review.
- **Version, date.** Open as of 2026-09-03 (Documented).

### 14. TiKV metadata GC used the local clock ([PR #6217](https://github.com/juicedata/juicefs/pull/6217))

- **What happened.** The client set TiKV's GC safe point from its own clock. TiKV versions are stamped by the PD server. If the client clock is ahead, "the safepoint could be advanced to a timestamp that disrupts transactional behavior" (Documented). Release note: "fix potential data corruption issues caused by clock diff and TiKV gc" (Documented).
- **Class.** Other, clock. A delete that read the wall clock. **Found:** review.
- **Fix.** Take the time from PD, the same source as the versions.
- **Version, date.** 2025-06-30. In v1.4.0-beta1 and v1.4.0 (Documented).

## 3. Retries and late requests

### 15. Many uploads of the same key at once ([#3260](https://github.com/juicedata/juicefs/issues/3260))

- **What happened.** The object store (IDrive e2) had a failure. With writeback on, JuiceFS kept retrying the upload of local blocks. The provider said: "We have noticed 10s of concurrent uploads for the same keys and that left the objects in inconsistent states" (Documented). `HEAD` worked, `GET` timed out, and `fsck` saw nothing wrong (Documented). The user deleted the affected files (Documented).
- **Class.** Client-side retries, plus a store that broke under concurrent PUTs to one key. **Found:** production.
- **Fix.** The maintainer pointed to [#3157](https://github.com/juicedata/juicefs/pull/3157), which reduces requests for the same object. It was on `main` but not in v1.0.3 (Documented).
- **Version, date.** v1.0.3, 2023-02.

### Late PUT after a delete: a leak, not a loss ([PR #4748](https://github.com/juicedata/juicefs/pull/4748))

A writeback upload that finishes after its slice was deleted recreates a dead object. Users saw this in production (Documented, see case 6). It costs storage. It does not lose data, because the slice ID is never used again within one history (Inferred). The cleanup added for it then caused case 6.

### What I did not find

- No issue where a late or retried DELETE removed data. I searched "late delete", "retry delete", "NoSuchKey", "slice id", "nextChunk", "reuse", "after load", "rollback", "clock", "split brain" and "stale session" in issues, PRs and discussions.
- No issue blaming S3 LIST lag or eventual consistency for lost data. The one hit, [#7272](https://github.com/juicedata/juicefs/issues/7272), is the opposite: deleted chunks stayed readable in a test.
- An open request for one retry layer that only retries "when it is safe to do so" ([#7455](https://github.com/juicedata/juicefs/issues/7455)) shows that retries are still spread over the callers (Documented).

## 4. Hazards without a reported loss

### 4.1 Compaction deleting slices a reader still uses

- [#4012](https://github.com/juicedata/juicefs/issues/4012): trash off. Client B compacts a file and deletes the old slices at once. Client A reads again with its cached slice list and gets EIO (Documented). The maintainer pointed to the existing invalidate-and-retry in the reader (Documented).
- [#6000](https://github.com/juicedata/juicefs/issues/6000): the same EIO during compaction with writeback on, v1.0.6 or earlier. Still open (Documented).
- [#2562](https://github.com/juicedata/juicefs/issues/2562): compaction with writeback updated metadata *before* uploading the new object, so other clients read a missing object (Documented). Fixed by [PR #5767](https://github.com/juicedata/juicefs/pull/5767), "disable writeback during compaction", in v1.3.0 (Documented). If the compacting client had crashed before the upload, the data would have been lost (Inferred).

All three are temporary read errors. All three need trash off or writeback on. With trash on, compacted slices wait `trash-days` before deletion (prior report, section 2), so a slow reader still finds them (Inferred).

### 4.2 Metadata backup and restore

- **Names collide after a restore.** A user loaded an hourly dump into a second volume as a "snapshot" and asked whether both could be written. Maintainer answer: both "allocate slice_ids from the same nextChunk starting point (restored by load), so they will collide on the same object keys and silently overwrite each other. ... This is a fundamental limitation of JuiceFS's current object-naming design" ([discussion #7359](https://github.com/juicedata/juicefs/discussions/7359), 2026-08-06) (Documented). This confirms the ID reuse the prior report found in source. Nobody reported losing data from it.
- **Backups are not one snapshot.** On SQL, the tree, nodes, chunks and xattrs are read in different transactions, so "a backup with a valid format cannot always be restored consistently" ([#7431 SQL-H02](https://github.com/juicedata/juicefs/issues/7431), open, translated) (Documented). The docs already say `dump` "does not provide snapshot consistency" (prior report).
- **Dumps drop the delayed slices.** After `load`, `gc` finds leaked objects ([#6230](https://github.com/juicedata/juicefs/issues/6230), open). This was a choice: "better not load the delSlices, because the whole sliceRefs is rebuild ... Instead we can just run `gc --delete` command after loading" ([PR #1790](https://github.com/juicedata/juicefs/pull/1790)) (Documented). Leak only.
- **Backup rotation may delete backups it should keep** ([#6783](https://github.com/juicedata/juicefs/issues/6783), open). The maintainer says there are gaps, "but there will be no accidental deletions" (Documented). Backups, not data.

### 4.3 Excluded

Not delete-related, or not the storage engine: zeros read during a concurrent write, which the maintainer says is not a bug ([#5038](https://github.com/juicedata/juicefs/issues/5038)). A read returning another block's data ([#7499](https://github.com/juicedata/juicefs/issues/7499)) and zstd overruns ([#7555](https://github.com/juicedata/juicefs/issues/7555)), which are read-path bugs. The S3 gateway deleting a sibling object ([#7426](https://github.com/juicedata/juicefs/issues/7426)). `juicefs sync` deletes ([#6125](https://github.com/juicedata/juicefs/issues/6125)). Writeback staging stalls ([#7558](https://github.com/juicedata/juicefs/issues/7558), [#7589](https://github.com/juicedata/juicefs/issues/7589), [#6161](https://github.com/juicedata/juicefs/issues/6161)). Leak-only bugs ([#5132](https://github.com/juicedata/juicefs/issues/5132), [PR #931](https://github.com/juicedata/juicefs/pull/931), [PR #904](https://github.com/juicedata/juicefs/pull/904), [#5194](https://github.com/juicedata/juicefs/issues/5194)).

## 5. Rules JuiceFS adopted because of these cases

| Rule | Where | Why | Ours |
|---|---|---|---|
| Keep compacted slices for `trash-days`, so an older metadata backup still works | [#1521](https://github.com/juicedata/juicefs/issues/1521), [PR #1790](https://github.com/juicedata/juicefs/pull/1790), v1.0.0-beta3 | "Better data safety in case users want to recover from an old backup of metadata" (Documented) | Have it: trash rows wait longer than copy retention ([#590](https://github.com/djosh34/s3-smb/issues/590), [#597](https://github.com/djosh34/s3-smb/issues/597)) |
| `gc` skips objects younger than 1 hour | [#2987](https://github.com/juicedata/juicefs/issues/2987) | Uploads still in flight look like leaks (Documented) | Have it in a stronger form: no automatic sweep. A future manual command needs it |
| Take that cutoff *before* listing live slices | [PR #5068](https://github.com/juicedata/juicefs/pull/5068) | Case 3 | Should have, in any future sweep |
| Any read error aborts `gc` | [PR #6481](https://github.com/juicedata/juicefs/pull/6481), [PR #7411](https://github.com/juicedata/juicefs/pull/7411) | Cases 1 and 2 | Should have, in any future sweep |
| Do not delete an upload whose commit outcome is unknown | [PR #4808](https://github.com/juicedata/juicefs/pull/4808) (rejected), then [PR #5133](https://github.com/juicedata/juicefs/pull/5133) deletes only on `ENOENT` | "It's possible that the `Write` actually succeeded, in which case deleting the objects will corrupt the file" (Documented) | Have it: uncommitted uploads are deleted only after retention plus 1 day (#590) |
| Claim a delayed row before lowering its count | [PR #7449](https://github.com/juicedata/juicefs/pull/7449) | Case 5 | Not needed with one writer (Inferred) |
| Side effects of a transaction only after commit | [PR #7460](https://github.com/juicedata/juicefs/pull/7460), [PR #7477](https://github.com/juicedata/juicefs/pull/7477) | Cases 7 and 8 | Should have, see section 6 |
| Time for GC comes from the same source as the versions | [PR #6217](https://github.com/juicedata/juicefs/pull/6217) | Case 14 | Have it if rule 5 of #595 holds: count copies, never read the clock |

## 6. Our exposure

All Inferred. "Our design" is the one on [#572](https://github.com/djosh34/s3-smb/issues/572): a new random name for every upload ([#586](https://github.com/djosh34/s3-smb/issues/586)), SQLite on local disk with a full copy to S3 every 15 minutes, a trash table filled in the same commit (#590), one server per bucket ([#584](https://github.com/djosh34/s3-smb/issues/584)), and #597 option X (fixed wait, no automatic sweep).

**Not exposed (10):**
- **Sweeps, cases 1 to 4.** We never delete an unknown chunk automatically. If a manual cleanup command is added later, it inherits all four. It must read the live set in one SQLite read transaction, take the cutoff before reading, stop on any error, and also respect chunks that kept copies point at.
- **Case 5.** Only one process runs GC.
- **Case 6.** We have no "delete my upload if nobody needs it" step. Each upload has its own name, so two uploads can never share a key.
- **Cases 9, 10.** No hard links and no open-unlinked files in a Time Machine-only server. That is assumed, not checked against the SMB layer.
- **Case 11.** One client and one writer. Every namespace change and its trash row are one SQLite commit.
- **Case 14.** Rule 5 of #595: deletes count copies, they never read the clock.
- **Collisions after a restore (4.2).** Random names are never reused.
- **Backups that are not one snapshot (4.2).** `VACUUM INTO` gives one consistent file (rule 1 of #595).

**Partly exposed (5):**
- **Cases 7 and 8, transaction retries.** SQLite can return `SQLITE_BUSY` and the code may retry. Any Go state written inside the transaction must be thrown away on retry. The safe shape: background deletes read only committed trash rows, never an in-memory list built during the transaction. Then a rolled-back attempt cannot cause a delete.
- **Case 12, a trash wait computed wrong.** The class applies to us. Our wait is "700 newer copies" instead of a timestamp, but a wrong copy number would have the same effect. That is why rule 1 of #595 matters. A test should check that a trash row survives until exactly N newer copies exist.
- **Case 13, a stale second server.** The bucket lock is best effort without conditional writes (#584). A paused server that resumes after another took over is the same shape as SQL-C04. JuiceFS chose availability here. We chose "one server per bucket", so the old server must stop for good once its lock is gone.
- **Case 15, duplicate PUTs of one key.** A retry of one upload reuses its name. If the earlier try is still in flight, two PUTs of the same key and the same bytes race. On a healthy store that is harmless. #3260 shows a store that broke under it. Keep at most one request per key in flight, and wait for the earlier one to return or time out before retrying.

## 7. Clever or blunt

**Caused by a clever cleanup (9):** cases 1, 2, 4 (sweep: "not in the live list means garbage"), 5, 7, 9, 10 (reference and link counts), 6 (delete my upload if not needed), 8 (a repair path that turns off trash).

**Neither (2):** case 11 (a create racing a directory move to trash; the wait only finished the job) and case 15 (aggressive re-upload).

**Caused by a blunt rule done wrong (3):** case 3 (fixed 1 hour skip, measured from the wrong moment), case 12 (fixed wait, broken timestamp), case 14 (fixed window, wrong clock). Case 13 is also a blunt timeout, and a design choice.

**Prevented by a blunt rule:** the compaction EIO cases (4.1) only happen with trash off. Restoring an older backup only works because of the fixed wait for compacted slices (#1521). The rejected PR #4808 kept an object rather than guess.

**What this says about #597 (Inferred).** JuiceFS's history supports option X. Its real losses come from bookkeeping that tries to prove an object is dead: counts, lists and "is it still pending". The fixed wait failed only when its start time was wrong. The worst production loss came from the leak sweep, which X leaves out. What X still needs from #595 is exactly what the blunt cases teach: a correct copy number (rule 1) and no clock (rule 5).

## Search method

- `gh search issues` and `gh search prs` on `juicedata/juicefs` for: data loss, lost, corrupt, data corruption, NoSuchKey, object not found, missing block, gc delete, gc --delete, trash, delslices, delayed slice, deleteSlice, delete slice, chunk_ref, refs, compact, compaction, slice id, nextChunk, load dump, restore metadata, metadata backup, dump, rollback, orphan, leaked, sustained, stale session, CleanStaleSessions, writeback, staging, upload failed, eventual consistency, versioning, fsck, EIO, inconsistent, retry, overwrite, clock, unexpected data was deleted, mistakenly delete, wrongly deleted.
- GitHub discussions on the same repo: data loss, lost data, gc delete, trash, objects deleted, metadata restore, load backup, missing object, NoSuchKey.
- All release notes from v1.0.0-beta1 to v1.4.1 and v1.3.3, filtered for delete, gc, trash, compact, leak, lost, corrupt, mistake.
- Upstream docs at `adcca1cc` (`docs/en`), for leak, data loss and corrupt.
- I did not search the JuiceFS Slack or the Chinese forum. Several Chinese-language issues came up through GitHub search and are included.
