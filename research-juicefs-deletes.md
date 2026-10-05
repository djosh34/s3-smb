# Research: How JuiceFS deletes data and lives with late requests

Ticket: [#596](https://github.com/djosh34/s3-smb/issues/596). Map: [#572](https://github.com/djosh34/s3-smb/issues/572). Context: [#592](https://github.com/djosh34/s3-smb/issues/592), [#595](https://github.com/djosh34/s3-smb/issues/595).

## Labels

- **Docs**: stated in the JuiceFS docs at the pinned commit, or in s3-smb's own docs.
- **Source**: read in code. Paths under `internal/` are this repo at `a38a46b`. `upstream/...` links are JuiceFS at the vendored commit `0b90c7db` (v1.4.1).
- **Issue**: a JuiceFS GitHub issue or maintainer comment.
- **Inferred**: my reasoning. Not tested.

The vendored copy is patched (see `docs/vendored.md`). Where s3-smb changed behaviour, this report says so.

## Short answer

JuiceFS uses one simple rule: **wait a fixed number of days, then delete.** Replaced and deleted data goes to a trash with a timestamp, and a background job deletes it after `trash-days` (default 1 day). Slice IDs come from a counter, so names are not reused within one history.

JuiceFS has the same problems we are fighting. It mostly does not solve them. It lives with them:

- Deletes are not linked to metadata backups at all. Backups are kept up to 2 years, but data is kept only 1 day. Older backups silently point at deleted data. The docs do not warn about this.
- A restore from a backup puts the ID counter back. New data then reuses IDs, and so object names, from the lost run.
- It does nothing about a late PUT or DELETE. It relies on "same name, same bytes" for PUT retries, and on IDs not coming back.
- Leaked objects are only removed by a manual `juicefs gc --delete`, which skips objects younger than 1 hour. Its known data-loss bugs are all in that command: an incomplete listing of live slices made it delete live data.

s3-smb v0.2.0 added a guard on top: no deletes at all unless a verified metadata backup is recent. It did not fix the ID reuse after restore.

## 1. Object names

**How names are built.** A file is cut into 64 MiB chunks. Each write becomes a *slice* with a new numeric ID. A slice is stored as blocks of up to 4 MiB under `chunks/<id/1e6>/<id/1e3>/<id>_<block index>_<block size>` (Source: `internal/juicefs/pkg/chunk/cached_store.go:76-81`). A slice is only as long as the bytes written, so a 4 KiB write becomes a 4 KiB object (Source: same file, `blockSize` at `:68-74` uses the slice length).

**How IDs are chosen.** `NewSlice` takes IDs from the SQL counter `nextChunk`, which is raised by 4096 at a time and handed out from memory (Source: `internal/juicefs/pkg/meta/base.go:53`, `:2145-2159`). Within one database, IDs only go up. The docs say each block "is assigned an unique ID" and "subsequent modifications on the file are carried out on new data blocks, and the original blocks remain unchanged" (Docs: `docs/en/guide/cache.md:22`).

**Can a name come back?** Yes, after a restore.
- `juicefs load` writes the counters from the dump, and only raises `nextChunk` above the highest slice ID *in the dump* (Source: [upstream `pkg/meta/dump.go:571-573`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/meta/dump.go#L571-L573)). IDs handed out after the backup are not known to it.
- s3-smb restores a SQLite snapshot, which holds the counter as it was (Source: `internal/backup/recovery.go`, no counter change). s3-smb's own docs say so: "Slice IDs allocated after the snapshot can be reused, so recovery must wipe the volume cache before publishing the database" (Docs: `docs/recovery.md`, "Recover on a new machine").
- Block size is part of the key, but every full block is 4 MiB, so a reused ID usually gives the exact same key (Inferred).
- I found no JuiceFS issue or doc about this (searched "nextChunk", "slice id reuse", "after load", "metadata rollback").

What it breaks (Inferred):
- A newer backup that was not chosen, for example when the user restores an older one on purpose, points at IDs above the restored counter. The new run reuses those IDs, overwrites those objects, and its compaction later deletes them. That newer backup is silently broken.
- A late PUT or DELETE from the lost run that lands after the restore hits a name that is live again. The window is small, because the old process is gone, but it is not zero.

## 2. When data is deleted

The design: the metadata row is changed first, in one transaction. Deleting objects in S3 happens later, in the background, and is tracked by rows so a failed delete is retried. A maintainer explained this: "It's slow to delete the data in object storage, so we can't put all these inside single transaction ... The deletion can fail, we will retry that by scanning the tracking records" (Issue: [#2519](https://github.com/juicedata/juicefs/issues/2519)).

### The tables

| Table | Holds | Source |
| --- | --- | --- |
| `chunk_ref` (`sliceRef`) | slice ID, size, reference count | `internal/juicefs/pkg/meta/sql.go:175-179` |
| `delslices` | replaced slices from compaction, with a delete timestamp | `sql.go:181-185` |
| `delfile` | a file whose data must be deleted, with a timestamp | `sql.go:233-237` |

### Overwrite

An overwrite does not delete anything. It appends a new slice to the chunk's slice list. The old slice stays referenced (Source: `doWrite`, `sql.go:3361-3404`). After 99, 199, ... or more than 350 slices in one chunk, a compaction starts (Source: `base.go:2191-2197`).

### Truncate

Truncate appends a "zero" slice (ID 0) over the cut range. No slice is freed (Source: `doTruncate`, `sql.go:1601-1671`). The old slices go away at the next compaction or file delete.

### Compaction

Compaction reads the slices, writes one new slice, then swaps the slice list in one transaction (Source: `compactChunk`, `base.go:2815-2927`, and `doCompactChunk`, `sql.go:3873-3955`).
- With trash on, the old slice IDs go into `delslices` with the current time. Their reference counts stay up (Source: `base.go:2895-2903`, `sql.go:3893-3899`).
- With trash off, their counts go down at once and slices with no references are deleted right away (Source: `sql.go:3899-3908`, `:3933-3950`).
- If the commit outcome is unknown, it reads `chunk_ref` to check whether the new slice landed (Source: `sql.go:3912-3926`). If the compaction lost a race, the new slice is deleted at once (Source: `base.go:2909-2911`). On other errors it logs that the slice "may be orphaned, will be cleaned by gc" (Source: `base.go:2914-2916`).

### File delete

- With trash on, `unlink` moves the file to `.trash/YYYY-MM-DD-HH/` (Docs: `docs/en/security/trash.md`, "Recover files"). The hourly trash job removes entries older than `24 * trash-days + 2` hours (Source: `doCleanupTrash`, `base.go:3292-3298`). Only then does the file get a `delfile` row.
- Otherwise, or after the trash, the inode gets a `delfile` row in the same transaction (Source: `sql.go:2079`). Its data is deleted at once in the background, at most 100 files at a time (Source: `tryDeleteFileData`, `base.go:2987-3003`). Leftovers are picked up by an hourly job, for rows older than 1 hour (Source: `cleanupDeletedFiles`, `base.go:1014-1048`).
- `deleteChunk` lowers the counts in one transaction, then deletes every slice whose count is 0 or less (Source: `sql.go:3732-3785`). There is no further delay at this point.

### Deleting a slice

`deleteSlice_` deletes the S3 blocks first, then the `chunk_ref` row (Source: `base.go:3018-3031`, `sql.go:566-574`). A crash in between leaves the row, and an hourly job retries every row with `refs <= 0` (Source: `cleanupSlices`, `base.go:1050-1080`; `doCleanupSlices`, `sql.go:3712-3730`). A DELETE of a missing key counts as success (Source: `cached_store.go:338-356`).

### Delayed slices

The hourly trash job also handles `delslices` older than `trash-days * 24h`: lower the counts, remove the row, delete slices that reach 0 (Source: `cleanupDelayedSlices`, `base.go:3300-3318`; `doCleanupDelayedSlices`, `sql.go:3809-3871`).

### Default timings

| Setting | Default | Source |
| --- | --- | --- |
| `trash-days` | 1 | Source: [upstream `cmd/format.go:195-197`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/cmd/format.go#L195-L197). Docs: `docs/en/administration/status_check_and_maintenance.md` shows `"TrashDays": 1` |
| Trash, delayed slice, deleted file and slice jobs | each about once an hour, with jitter, at most 50 minutes per run | Source: `base.go:1014-1080`, `:3100-3168`. Docs: `docs/en/security/trash.md`, "Permanently delete files" |
| Delete threads | 10 | Source: [upstream `cmd/flags.go:148-150`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/cmd/flags.go#L148-L150) |
| s3-smb `backup.trash_days` | 14 | Source: `internal/config/config.go:163` |

Everything is based on the wall clock. No delete looks at backups (Source: none of the paths above read backup state, upstream).

## 3. Late requests

JuiceFS does nothing special about a PUT or DELETE whose outcome it never saw.

- **PUT.** A block upload is retried with the same key and the same bytes, with `try^2` second sleeps, up to `MaxRetries + 1` times (default 10, s3-smb uses 12) (Source: `cached_store.go:381-397`, `:843-845`; `internal/storage/runtime.go:46-48`). Each try has a 60 s timeout (Source: `cached_store.go:849-851`).
- **The timeout does not stop the request.** `WithTimeout` returns at the deadline and cancels the context, but a request already sent may still be applied by the server later (Source: `internal/juicefs/pkg/utils/utils.go:112-129`; Inferred for the server side).
- **DELETE.** One try per call, with the same 60 s timeout. A failure leaves the `chunk_ref` row, so the hourly job tries again (Source: `cached_store.go:338-356`, `base.go:1050-1080`).
- **The S3 SDK does not retry.** Upstream sets `RetryMaxAttempts = 1` (Source: `internal/juicefs/pkg/object/s3.go:596`). s3-smb does the same: "native chunk layer owns data retries" (Source: `internal/juicefs/pkg/object/s3_smb.go:54`).
- An open upstream issue asks for one retry layer in `pkg/object`, with "retry only when it is safe to do so" (Issue: [#7455](https://github.com/juicedata/juicefs/issues/7455), open). So today retries are spread over callers.

Why it is mostly fine for JuiceFS (Inferred):
- A late PUT writes the same bytes under the same key. Harmless.
- A late DELETE hits a slice whose count is 0. Within one history that ID never comes back.
- An upload that never got committed is a leaked object. Nothing deletes it automatically.

Where it is not fine (Inferred): after a restore, because IDs come back (section 1).

## 4. Metadata backups

Upstream:
- Every mounted client backs up hourly by default, as `meta/dump-YYYY-MM-DD-HHMMSS.json.gz` in the bucket. A global timestamp makes only one client do it (Docs: `docs/en/administration/metadata_dump_load.md`, "Automatic backup"; Source: [upstream `pkg/vfs/backup.go:51-99`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/backup.go#L51-L99)).
- Above 1 million inodes with a 1 hour interval, the backup is skipped with a warning (Docs: same page; Source: [`backup.go:74-79`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/backup.go#L74-L79)).
- Retention: all for 2 days, one a day to 2 weeks, one a week to 2 months, one a month to 2 years (Docs: same page; Source: [`rotate`, `backup.go:176`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/backup.go#L176)).
- `juicefs dump` "does not provide snapshot consistency" (Docs: same page, note at the top).
- A failed backup is only logged. Deletes go on (Source: [`backup.go:87-93`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/backup.go#L87-L93)).

What happens to data when an old backup is restored:
- The backup only works while its slices still exist. With `trash-days = 1`, a backup older than about 1 day may point at deleted slices. Only overwritten or deleted data is affected (Inferred, from section 2).
- The docs do not warn about this on the backup page. The trash page says the opposite direction: stale slices are kept for the trash period, so "original state can be recovered through metadata backups" (Docs: `docs/en/security/trash.md`, "Trash and slices").
- A maintainer said the same: "It's recommend to keep trash on for data safety. For example, you can restore JuiceFS using backuped meta from hours ago without data corruption." This was on a Time Machine issue (Issue: [#3030 comment](https://github.com/juicedata/juicefs/issues/3030#issuecomment-1336037247)).
- Data written after the backup becomes leaked objects. IDs then get reused (section 1).

So the JuiceFS rule is simply: **the trash period is the restore window.** Nothing enforces it.

## 5. Leaked objects and `juicefs gc`

- **Manual only.** No background job removes leaked objects. `juicefs gc` only scans; `--delete` removes (Docs: `docs/en/administration/status_check_and_maintenance.md`, "gc"; `docs/en/reference/command_reference.mdx:336-363`).
- **How.** It lists all slices in the live metadata, plus trash and pending slices, then lists every object under `chunks/`. Any object whose ID is not in that set is leaked (Source: [upstream `cmd/gc.go:196-345`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/cmd/gc.go#L196-L345)).
- **Skips new objects.** Objects with a modified time in the last hour are skipped. `JFS_GC_SKIPPEDTIME` changes this (Source: [`gc.go:112-120`, `:307-311`](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/cmd/gc.go#L112-L120); Docs: same page). The reason given is uploads still in flight after an interrupted copy (Issue: [#2987](https://github.com/juicedata/juicefs/issues/2987)).
- **It only checks live metadata, not backups.** An object that only an older backup needs counts as leaked (Inferred, from the source above).
- The docs call leaks rare, and say a leak "without any special file system manipulation ... could well indicate a bug" (Docs: `command_reference.mdx:338`).
- The trash page tells users to set `trash-days 0`, run `gc --compact` and `gc --delete`, and turn trash back on, when stale slices get too big (Docs: `docs/en/security/trash.md`, "Trash and slices").
- s3-smb does not ship `gc`. Its docs say "There is no garbage-collection command for those objects" (Docs: `docs/recovery.md`).

## 6. What s3-smb v0.2.0 adds

| Addition | What it does | Why | Source |
| --- | --- | --- | --- |
| Protection guard | No delete or compaction unless the last verified backup started less than `interval + budget` ago (1 h + 1 h). Starts closed. Once expired it stays closed for the process | Upstream deletes even when backups fail for days. Without a recent backup, a delete could remove data the newest backup needs | `internal/backup/protection.go:14-74` |
| Guard everywhere | Checked in every delete, truncate and compaction transaction, again just before commit, in `deleteSlice_`, in trash cleanup and in the S3 `Delete` wrapper | One missed path would bypass it | `internal/juicefs/pkg/meta/protection.go:30-50`; `base.go:2816`, `:3018-3023`; `sql.go:3818-3820`; `internal/storage/runtime.go:78-117` |
| Trash days must exceed the backup window | `interval + budget < trash_days * 24h`, default 14 days | Data a recent backup needs must still be in the trash | `protection.go:38-40`; `internal/config/config.go:163` |
| Verified backups | SQLite snapshot, name reserved on local disk first so it is never reused, uploaded, read back and hashed. A receipt is saved | A lost reply must not let a name be reused. A backup that cannot be read back must not open the guard | `internal/backup/backup.go:190-275` |
| Retention | Same shape as upstream (2 days, daily to 2 weeks, weekly to 2 months, monthly to 2 years). Deletes go through the guard | Upstream behaviour kept | `internal/backup/retention.go:16-60` |
| Recovery | Checks hash, SQLite integrity and identity, cleans sessions, wipes the volume cache, then renames into place. Takes a new backup before serving | A reused ID must not read a stale cached block | `internal/backup/recovery.go`; `internal/backup/cache.go:14`; `docs/recovery.md` |

What it does not fix (Inferred):
- Backups older than 14 days can point at deleted data, yet are kept up to 2 years. `docs/recovery.md` says a recovery point "is usable only while the data blocks it points at still exist", and "Why retention matters" explains compaction. It does not say which backups are still whole.
- IDs reused after a restore can overwrite objects that a newer, unchosen backup needs (section 1).
- Late requests are not handled beyond what upstream does.

## 7. Known JuiceFS issues

| Issue | What | Status |
| --- | --- | --- |
| [#6479](https://github.com/juicedata/juicefs/issues/6479) | `gc --delete` deleted "a massive number of objects" still in use. Suspected cause: `ListSlices` ignored errors, so a partial list looked complete | Closed, fixed in #6481 |
| [#7530](https://github.com/juicedata/juicefs/issues/7530) | `gc` went on with a partial `jfs_chunk` read after the database dropped the connection. XORM returned success. Live objects could be deleted. The same XORM fork is vendored here (`internal/thirdparty/xorm`) | Closed, fixed upstream by an XORM upgrade (#7411) |
| [#6230](https://github.com/juicedata/juicefs/issues/6230) | `dump` does not write `delslices` and `chunk_ref` correctly. After `load`, gc finds leaked objects | Open |
| [#2987](https://github.com/juicedata/juicefs/issues/2987) | Interrupted uploads leak objects. Answer: gc skips the last hour | Closed |
| [#3030](https://github.com/juicedata/juicefs/issues/3030) | Time Machine on JuiceFS: 5.8 GB on disk, 46 GiB in the bucket after about an hour. Answer: trash plus in-place updates, run `gc --compact --delete` | Open |
| [#6161](https://github.com/juicedata/juicefs/issues/6161) | Possible data loss with writeback and upload delay | Closed, not a bug per maintainer |
| [#7455](https://github.com/juicedata/juicefs/issues/7455) | No unified retry layer for object storage | Open |

I found no issue about data loss from a late DELETE, from ID reuse after `load`, or from a backup pointing at data already deleted from the trash (searched "late delete", "slice id reuse", "nextChunk", "metadata rollback", "restore from backup", "trash days data loss").

## What this means for our GC rules

Inferred, for the owner to weigh.

1. **JuiceFS proves the fixed wait works in practice, at a storage cost.** Their whole model is option A from #592: a replaced or deleted object waits a fixed time, then goes. Its waste is what #3030 shows for Time Machine.

2. **JuiceFS wastes less per hot spot because its objects are small, not because its rule is smarter.** A 4 KiB write is a 4 KiB object. Compaction then rewrites the chunk, and that is where the cost returns. Our 8 MiB whole-chunk upload per FLUSH is what makes the fixed wait expensive, not the delete rule.

3. **Its gaps are ones our design already closes.**
   - ID reuse after restore: our random names, never reused ([#586](https://github.com/djosh34/s3-smb/issues/586)).
   - Backups older than the delete wait: our copies expire after 7 days and are never restored after that ([#590](https://github.com/djosh34/s3-smb/issues/590), [#593](https://github.com/djosh34/s3-smb/issues/593)).
   - Deletes while backups fail: v0.2.0's guard, and #591 with rule 7 of #595.

4. **It never deletes unknown objects automatically.** Leaks only go with a manual command and a 1 hour skip. Its real data-loss bugs are all in that one sweep, where an incomplete list of live objects looked complete.

5. **A dead simple version of our GC, in JuiceFS's spirit:**
   - Replaced and deleted chunks go to the trash table in the same commit (#590, as now).
   - Each trash row waits a fixed time longer than copy retention, then is deleted. That is option A of #592.
   - No automatic deletion of unknown chunks. Leaks after a crash or disk loss stay as wasted storage, or go with a manual command.

   With these three, rules 1, 4b and the upload half of rule 6 in #595 are not needed. No delete then depends on copy counters or on what a late copy may hold. A late copy taken before a chunk was replaced expires before that chunk is deleted. The read half of rule 6 is also covered, because nothing is deleted while a read of it can still be running.

   The wait can count copies instead of days, to keep rule 5: store the copy sequence number at replacement, and delete after 673 more copies.

6. **The price is storage**, about the "A" numbers on #592: about 100 GiB per hot 8 MiB chunk with hourly backups (an estimate from one trace). If that is too much, B-lite brings back the counter rules. The way to cut waste without them is smaller objects, which is a write path change, not a GC change.
