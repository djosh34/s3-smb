# Size of rewrite hot spots over several Mac backups

Ticket: [#598](https://github.com/djosh34/s3-smb/issues/598). Follows [#528](https://github.com/djosh34/s3-smb/issues/528) and its report on `research/tm-mac-trace`. Feeds [#592](https://github.com/djosh34/s3-smb/issues/592) and [#597](https://github.com/djosh34/s3-smb/issues/597). Status: done, 2026-10-05. Throwaway branch. Nothing here lands on `main`.

Labels: **M** measured in the trace, **C** computed from the trace with the model below, **E** extrapolated beyond the run.

## Short answer

- **Hot spots are real and steady.** Four 8 MiB chunks are rewritten in every backup: `bands/0` at 0 and 112 MiB, `mapped/0`, and `bands/5` at 240 MiB. `bands/0` chunk 0 gets 18 to 89 versions per backup. Up to 9 more chunks get hot in one backup and cool down later. **C**
- **Every incremental sends 0.4 to 2.7 GiB to the trash at 8 MiB chunks**, mean 1.4 GiB, even when only 40 KiB of user data changed (inc5: 0.8 GiB). **C**
- **Trash per option, hourly backups, 8 MiB chunks:** A (7 days) about 233 GiB. B-lite (7 days) about 44 GiB. Short wait (5 copies, about 75 minutes) 2.8 GiB on average and 4.3 GiB at worst, and it does not grow with time. **E**
- **JuiceFS would upload 1.7 to 20 times fewer bytes** per incremental than 8 MiB chunks: 55 to 570 MiB against 0.8 to 2.7 GiB. **C**
- **Smaller chunks cut the trash 4 to 9 times:** short wait peaks about 1.0 GiB at 1 MiB and 0.4 GiB at 256 KiB. Rows grow 8 and 31 times. **C, E**

## Run

[Run 37361509487](https://github.com/djosh34/s3-smb/actions/runs/37361509487), commit 49f7c66, `server=smbnext`, `mode=scenarios`, `scenarios=["hotspots"]`. One job, passed in 32 minutes of test time. Runner `macos-15-intel`, image macos-15 20260824.0482.1, macOS 15.7.9 (24G830), x86_64. "Encrypt backups" off. Band size 256 MiB (backupd log).

The branch is `research/tm-mac-trace` plus one commit. It is not rebased onto `main`. The trace sits at the SMB layer, so it shows what macOS sends, whatever the storage under it. The server under the trace is still the JuiceFS-backed `smbnext`.

### Harness change

A new `hotspots` scenario (`test/macos/hotspots_test.go`):

1. The usual small test tree, plus a big tree of random data: 22,511 files, 5,766,250,496 bytes (5.4 GiB).

   | Directory | Files | Size each |
   |---|---:|---:|
   | `large` | 3 | 1 GiB |
   | `medium` | 8 | 128 MiB |
   | `photos` | 100 | 8 MiB |
   | `docs` | 400 | 1 MiB |
   | `code` | 2,000 | 64 KiB |
   | `notes` | 20,000 | 4 KiB |

2. First backup, then 5 incremental backups. Before each one, the tree changes as below. Each backup is listed with `tmutil listbackups` and checked to be new. No restores, to save time.
3. Budgets raised to 190 minutes. The harness logs `hotspots-backup-start` and `hotspots-backup-end` with exact times, and saves `hotspots-changes.json`.

| Backup | What changed before it | Added | Changed files (their size) | Deleted |
|---|---|---|---|---|
| baseline | first backup | 22,511 files, 5.4 GiB | | |
| inc1 | typical: 2% of notes and code edited, 5 photos grown by 1 MiB, 100 code files and one 128 MiB file added, 1% of notes and 2 photos deleted | 101 files, 134 MiB | 445 (49 MiB) | 202 files, 17 MiB |
| inc2 | large: 16 MiB overwritten inside one 1 GiB file, 500 MiB added, one 128 MiB file deleted | 150 files, 500 MiB | 1 (1 GiB) | 1 file, 128 MiB |
| inc3 | typical: 2% of notes and code and 10 docs edited, 20 photos added, 1% of notes deleted | 20 files, 160 MiB | 450 (14 MiB) | 200 files, 0.8 MiB |
| inc4 | delete-heavy: one 1 GiB file, 20% of notes and 10% of code deleted, 50 MiB added | 50 files, 50 MiB | 0 | 4,201 files, 1.0 GiB |
| inc5 | tiny: 10 notes edited | 0 | 10 (40 KiB) | 0 |

## Model

`research-tm-hotspots/hotspots.py` replays the trace into a chunk table, as the design in #592 and #597 would keep it:

- A WRITE marks the chunks it touches dirty. A FLUSH of that file uploads each dirty chunk as a new version under a new name. A chunk object is the chunk size, or less for the last chunk of a file.
- The version it replaces goes to the trash at that moment. Deletes, truncates and overwriting CREATEs send the dropped chunks to the trash. Renames move chunks.
- A CLOSE with dirty chunks and no FLUSH would also upload. It never happened. All 1,407 committing requests were FLUSHes. **M**
- Paths are taken inside the bundle, so the `.incomplete` rename does not matter.

Each version's write and replace times come from the trace. Database copies are on a 15-minute grid. Because the grid's phase is arbitrary, every copy number is the mean of 15 grids, one minute apart, with min and max.

Trash options:

- **A:** every replaced version kept 7 days.
- **B-lite:** a version that no copy saw (no copy between its write and its replace) is deleted at once. The rest are kept 7 days.
- **Short wait** (owner's decision on #597): keep the last 4 copies. A replaced version is deleted when 5 newer copies have landed, 60 to 75 minutes after it was replaced. Every replaced version waits, seen by a copy or not.

### Two timelines

The CI backups ran back to back, 1 to 2 minutes apart, not hourly. That matters for B-lite and short wait:

- **As run:** the real trace times. The whole run is 25 minutes, so short wait deletes nothing before the end, and B-lite rarely sees a copy.
- **Hourly (C):** the same events, with backup *i* moved to start at hour *i*. Times inside each backup are kept. Then the version left at the end of one backup lives an hour, so a copy always sees it. This is the realistic case for B-lite and short wait. It assumes macOS would write the same things if the gap were an hour. **C, with that assumption**

### What is left out

- **Harness traffic between backups.** The harness attaches the image and lists backups after each backup. That wrote 2.7 MiB and replaced 810 MiB of 8 MiB chunk versions, mostly the same hot chunks (column "other" or "harness" below). A real Mac does not do this. It is left out of every per-backup and extrapolated number.
- **Database copies themselves** (their size is in #592).

## Results

### Writes per backup (M)

| Backup | Seconds | WRITEs | Bytes written (MiB) | of which zeros |
|---|---:|---:|---:|---:|
| baseline | 205 | 9,650 | 6,478 | 897 |
| inc1 | 87 | 1,039 | 223 | 0 |
| inc2 | 66 | 1,019 | 570 | 0 |
| inc3 | 288 | 2,305 | 248 | 0 |
| inc4 | 293 | 2,443 | 138 | 0 |
| inc5 | 87 | 430 | 55 | 0 |
| harness, between backups | | 262 | 2.7 | 0 |

- FLUSH: 1,809 `data` and 450 `full` over the whole job.
- Only the first backup writes zeros (the APFS erase, as in #528).
- inc2 wrote 570 MiB for a tree change of 500 MiB new files plus a 1 GiB file with 16 MiB changed. So Time Machine sent only part of the changed 1 GiB file, not all of it. **I**
- inc5 changed 40 KiB of user data and wrote 55 MiB. The rest is Time Machine's own data and Mac files outside the exclusion list. **I**

### 1. Chunk versions uploaded (C)

| Backup | 8 MiB versions | 8 MiB MiB | 1 MiB versions | 1 MiB MiB | 256 KiB versions | 256 KiB MiB | JuiceFS MiB |
|---|---:|---:|---:|---:|---:|---:|---:|
| baseline | 1,156 | 7,778 | 6,917 | 6,721 | 26,362 | 6,541 | 6,478 |
| inc1 | 291 | 1,561 | 579 | 477 | 1,291 | 299 | 223 |
| inc2 | 158 | 962 | 686 | 647 | 2,409 | 593 | 570 |
| inc3 | 324 | 1,896 | 685 | 590 | 1,583 | 372 | 248 |
| inc4 | 476 | 2,804 | 816 | 690 | 1,459 | 334 | 138 |
| inc5 | 129 | 794 | 216 | 187 | 424 | 99 | 55 |
| harness | 136 | 810 | 184 | 147 | 209 | 44 | 2.7 |
| total | 2,670 | 16,604 | 10,083 | 9,458 | 33,737 | 8,283 | 7,715 |

- Per band FLUSH at 8 MiB, the median upload is 1 version in incrementals and 2 in the first backup. The most is 32 (a full band of bulk data). In incrementals, 58% to 69% of band FLUSHes upload just 1 chunk.
- Upload at 8 MiB is 1.7 to 20 times the bytes written in an incremental. At 256 KiB it is 1.0 to 2.4 times.

### 2. Hot spots (C)

Versions per 8 MiB chunk, top 15 by versions in all backups:

| File | Offset (MiB) | baseline | inc1 | inc2 | inc3 | inc4 | inc5 | harness |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| `bands/0` | 0 | 89 | 51 | 20 | 46 | 60 | 18 | 42 |
| `mapped/0` | 0 | 49 | 36 | 11 | 28 | 42 | 9 | 18 |
| `bands/0` | 112 | 47 | 36 | 11 | 28 | 42 | 9 | 18 |
| `bands/5` | 240 | 37 | 24 | 3 | 20 | 31 | 8 | 15 |
| `bands/a` | 48 | 1 | 6 | 7 | 15 | 30 | 7 | 10 |
| `bands/e` | 56 | 1 | 8 | 0 | 11 | 22 | 5 | 4 |
| `mapped/5` | 0 | 35 | 7 | 1 | 1 | 1 | 0 | 3 |
| `bands/6` | 32 | 3 | 7 | 6 | 10 | 16 | 1 | 1 |
| `mapped/1f` | 0 | 0 | 0 | 0 | 6 | 31 | 6 | 2 |
| `mapped/e` | 0 | 1 | 12 | 0 | 10 | 9 | 3 | 2 |
| `bands/e` | 64 | 1 | 5 | 0 | 7 | 12 | 6 | 1 |
| `bands/1c` | 88 | 0 | 4 | 1 | 5 | 15 | 4 | 2 |
| `mapped/1e` | 0 | 0 | 0 | 1 | 12 | 12 | 2 | 1 |
| `mapped/12` | 0 | 3 | 9 | 0 | 6 | 3 | 2 | 0 |
| `bands/12` | 64 | 1 | 5 | 0 | 8 | 6 | 3 | 0 |

Chunks with 10 or more versions in one backup:

| Chunk size | baseline | inc1 | inc2 | inc3 | inc4 | inc5 |
|---|---:|---:|---:|---:|---:|---:|
| 8 MiB | 6 (271 versions) | 5 (159) | 3 (42) | 9 (180) | 13 (334) | 1 (18) |
| 256 KiB | 17 (383) | 11 (189) | 3 (33) | 5 (119) | 11 (241) | 1 (18) |
| 4 KiB | 6 (196) | 4 (96) | 2 (22) | 6 (100) | 6 (170) | 0 |

- **The same four spots are hot in every backup:** `bands/0` at 0 (the container superblock and checkpoint area, #528), `bands/0` at 112 MiB, `mapped/0` and `bands/5` at 240 MiB. They are the same spots #528 found in its 4 MB run. `mapped/0` and `bands/0` at 112 MiB move in lockstep.
- **Other spots move.** `bands/a`, `bands/e`, `mapped/1f`, `mapped/1e` and `bands/1c` get hot in later backups. APFS is copy-on-write, so its metadata moves.
- **Hot spots grow with the work, not with the data.** inc4 deleted 4,201 files and wrote 138 MiB, and it has the most hot chunks (13) and the most trash. inc2 added 500 MiB and has the fewest.
- **Not all trash is hot spots.** At 8 MiB, chunks with 10 or more versions give 25% to 69% of an incremental's trash. The rest is chunks rewritten 2 to 9 times, for example a band chunk filled over several FLUSH rounds.

### 3. Trash per backup

Bytes sent to the trash (C). The same bytes go to the trash under every option. The options differ in how long they stay.

| Backup | 8 MiB | 1 MiB | 256 KiB | 8 MiB, from hot chunks | Dropped files (all sizes) |
|---|---:|---:|---:|---:|---:|
| baseline | 1,808 MiB | 751 MiB | 572 MiB | 1,203 MiB | 505 MiB |
| inc1 | 1,431 MiB | 347 MiB | 169 MiB | 870 MiB | 0 |
| inc2 | 444 MiB | 129 MiB | 76 MiB | 253 MiB | 0 |
| inc3 | 1,713 MiB | 407 MiB | 190 MiB | 1,017 MiB | 0 |
| inc4 | 2,737 MiB | 623 MiB | 267 MiB | 1,889 MiB | 0 |
| inc5 | 778 MiB | 171 MiB | 83 MiB | 192 MiB | 0 |
| mean of inc1 to inc5 | 1,421 MiB | 336 MiB | 157 MiB | 844 MiB | 0 |

The first backup's dropped bytes are the zero-filled bands that DiskImages deletes during the APFS erase (#528). No band was deleted in any incremental. **M**

How much of it stays, per option (MiB):

| Backup | A, kept 7 days, 8 MiB | B-lite as run, 8 MiB | B-lite hourly, 8 MiB | B-lite hourly, 1 MiB | B-lite hourly, 256 KiB |
|---|---:|---:|---:|---:|---:|
| baseline | 1,808 | 7 (0 to 39) | 7 (0 to 39) | 2 | 1.5 |
| inc1 | 1,431 | 66 (0 to 305) | 309 (305 to 363) | 140 | 113 |
| inc2 | 444 | 19 (0 to 107) | 108 (107 to 131) | 66 | 59 |
| inc3 | 1,713 | 141 (0 to 289) | 324 (289 to 462) | 127 | 94 |
| inc4 | 2,737 | 143 (8 to 335) | 385 (335 to 562) | 143 | 108 |
| inc5 | 778 | 59 (0 to 202) | 209 (202 to 306) | 84 | 61 |

- **B-lite as run** keeps little, because back-to-back backups rarely span a copy. This is a CI artifact. **C**
- **B-lite hourly** keeps about 1 version per hot chunk per copy that falls in the backup, plus the version left between backups. That is 108 to 385 MiB per backup at 8 MiB. **C**
- **Short wait** holds every replaced version for 60 to 75 minutes, then deletes it. Peak bytes held at one time (GiB):

| Chunk size | As run, whole job | Hourly, whole job | Hourly, incrementals only |
|---|---:|---:|---:|
| 8 MiB | 8.7 | 4.2 (3.4 to 4.3) | 4.2 (3.4 to 4.3) |
| 1 MiB | 2.4 | 1.1 (0.9 to 1.1) | 1.0 (0.8 to 1.0) |
| 256 KiB | 1.3 | 0.7 (0.7 to 0.7) | 0.4 (0.4 to 0.4) |

As run, the whole 25-minute job fits inside one wait, so nothing is deleted and the peak is all the trash. Hourly, the peak is about one backup plus the next: inc3 plus inc4 is 4.35 GiB at 8 MiB. **C**

### Extrapolated to hourly backups over 7 days (E)

Assumptions:

- One incremental per hour for 168 hours, each like the mean of inc1 to inc5. The mean includes a large and a delete-heavy backup. inc5 (0.8 GiB) is closer to an idle hour.
- Each backup takes under 45 minutes, so under short wait at most two backups' trash is held at once.
- Copies land on time every 15 minutes. B-lite uses the hourly per-backup numbers above.
- No thinning by Time Machine, no band deletes, no encryption, one Mac. The same 4 to 13 hot chunks per backup as in this run.

| Chunk size | Trash per hour | A, 7 days | B-lite, 7 days | Short wait, mean (2 backups) | Short wait, worst 2 in a row |
|---|---:|---:|---:|---:|---:|
| 8 MiB | 1,421 MiB | 233 GiB | 44 GiB | 2.8 GiB | 4.3 GiB |
| 1 MiB | 336 MiB | 55 GiB | 18 GiB | 0.7 GiB | 1.0 GiB |
| 256 KiB | 157 MiB | 26 GiB | 14 GiB | 0.3 GiB | 0.4 GiB |

- Under A and B-lite, the trash is 168 times the hourly amount. Under short wait, it stays at one or two backups' worth, whatever the retention of the backups.
- #592 guessed about 100 GiB per hot chunk under A and about 5 GiB under B. The measured total is 233 GiB under A and 44 GiB under B-lite, for all chunks together. Hot chunks give about 60% of it.
- #597 guessed "under 1 GiB" for short wait. At 8 MiB the measured mean is 1.4 GiB per backup, so 2.8 GiB held on average and 4.3 GiB at worst. At 1 MiB it is under 1 GiB.

### 4. What JuiceFS would upload (C)

JuiceFS uploads only the bytes written, as slices, at each FLUSH. Counted as the sum of WRITE lengths between FLUSHes. Overlapping writes in one FLUSH interval were almost absent: 40 KB in the first backup, none later. Compaction and its trash are not counted.

| Backup | JuiceFS | Ours, 8 MiB | Ratio | Ours, 1 MiB | Ours, 256 KiB |
|---|---:|---:|---:|---:|---:|
| baseline | 6,478 MiB | 7,778 MiB | 1.2 | 6,721 MiB | 6,541 MiB |
| inc1 | 223 MiB | 1,561 MiB | 7.0 | 477 MiB | 299 MiB |
| inc2 | 570 MiB | 962 MiB | 1.7 | 647 MiB | 593 MiB |
| inc3 | 248 MiB | 1,896 MiB | 7.6 | 590 MiB | 372 MiB |
| inc4 | 138 MiB | 2,804 MiB | 20.3 | 690 MiB | 334 MiB |
| inc5 | 55 MiB | 794 MiB | 14.4 | 187 MiB | 99 MiB |

### 5. Database rows at the end (C)

Live chunk rows after inc5, over the 61 non-empty files in the bundle (bands, `mapped/`, plists, `Info.*`). The image holds 6,882 MiB. Every chunk inside every file was written at least once, so there are no holes.

| Chunk size | Live chunk rows | Per TiB of image (E) |
|---|---:|---:|
| 8 MiB | 895 | about 136,000 |
| 1 MiB | 6,916 | about 1.05 million |
| 256 KiB | 27,561 | about 4.2 million |

The per-TB numbers scale linearly and match the estimates in #592.

### 6. Local scratch until FLUSH, 8 MiB chunks (C)

Asked after the first report. Model (`research-tm-hotspots/scratch.py`): every dirty chunk waits in a local scratch folder until its file gets a FLUSH or CLOSE. Then it is uploaded and leaves scratch at once. A dirty chunk takes min(8 MiB, file size minus chunk start), even if only 4 KiB of it was written. "Sparse" counts a chunk as 0 bytes while it holds only zeros (all its writes were zeros, and it never had nonzero data).

Bytes in scratch at the same moment (MiB). p99 is over time: the level that is exceeded only 1% of the time. Same run and trace as above.

| Phase | Peak | p99 | Peak, sparse zeros | p99, sparse zeros | Peak at (UTC) |
|---|---:|---:|---:|---:|---|
| whole run | 1,232 | 953 | 1,224 | 951 | 19:20:06, first backup |
| first backup, APFS erase | 248 | 248 | 22 | 22 | 19:19:17 |
| first backup, copy | 1,232 | 1,217 | 1,224 | 1,209 | 19:20:06 |
| inc1 | 128 | 64 | 128 | 64 | 19:24:48 |
| inc2 (500 MiB added) | 353 | 345 | 353 | 345 | 19:26:58 |
| inc3 | 152 | 115 | 152 | 115 | 19:33:22 |
| inc4 | 112 | 48 | 112 | 48 | 19:37:12 |
| inc5 | 90 | 64 | 90 | 64 | 19:41:54 |
| harness, between backups | 56 | 0 | 56 | 0 | 19:43:08 |

- **The peak is the bulk copy of the first backup.** From 19:19:39 to 19:20:06 macOS wrote 1,140 MiB to bands without any band FLUSH, about 42 MiB/s for 27 seconds. Then it FLUSHed the bands one at a time, 2 to 5 seconds apart, so the later bands waited longer. At the peak, 7 bands (`bands/6` to `bands/c`) held about 150 dirty chunks, almost all with real data. **M** for the writes and FLUSHes, **C** for the bytes.
- **Sparse zeros only help during the APFS erase.** There it cuts the peak from 248 MiB to 22 MiB. In the copy phase all dirty chunks hold data, so it saves 8 MiB.
- **In incrementals, scratch stays under 360 MiB.** The largest is inc2, which copied 500 MiB of new files. Hot-spot chunks add little, because they are FLUSHed within seconds.
- **The peak follows the data written between FLUSHes, not the tree size.** Here that was 27 seconds at 42 MiB/s. A faster link or disk, or a client that FLUSHes less often, would give a higher peak. A first backup of a real Mac may write for longer between FLUSHes. This run does not show an upper limit. **I**

## 7. Larger tree, wide incrementals (second round)

Asked after the first report: a much larger tree, and incrementals that change 10% to 30% of all files. Two Mac runs, same model and scripts as above. Labels as before.

### Runs and runner limits

| Run | Tree | Backups | Result |
|---|---|---|---|
| [37380880797](https://github.com/djosh34/s3-smb/actions/runs/37380880797), `hotspots-large` | 15.7 GiB, 85,390 files (scale 0.585, sized from free disk) | first, inc1, inc2 complete. inc3 stopped after 30 minutes | Failed: free disk below 20 GiB during inc3 |
| [37393029984](https://github.com/djosh34/s3-smb/actions/runs/37393029984), `hotspots-medium` | 7.0 GiB, 36,464 files (scale 0.25, fixed) | first and 5 incrementals | Passed, 2 h 7 min of test time |

Both: `macos-15-intel`, image 20260824.0482.1, macOS 15.7.9, encryption off, MinIO, server `smbnext`.

What was tried, and why the tree was cut:

1. **Disk.** The runner starts with about 112 GB free. A new workflow step deletes the extra Xcode copies, simulator runtimes, Android SDK and .NET. That frees about 60 GB, to 172 GB, and takes 11 minutes. **M**
2. **The store grows much faster than the backup.** MinIO under the JuiceFS-backed test server grew 5 to 7 times the bytes macOS wrote: 126 GiB of objects for about 17 GiB written in the medium run. During the large run's inc3 it grew 29 GB while Time Machine copied 3.5 GB. This is the old engine's write amplification, not Time Machine. **M**
3. **The large run** sized its tree from free disk, assuming 6.5 times the tree. That was too little. inc3 hit the harness's 20 GiB floor at 19% done, and Time Machine's speed had dropped to about 0.3 MB/s. Its first backup, inc1 and inc2 are complete and used below. inc3 is used as a partial window up to the failure (backupd "Failed copy" at 00:09:24).
4. **The medium run** used a fixed quarter-size tree with the same changes. It passed, but ended with 20.5 GB free, so this is about the largest that fits. **M**
5. **Time.** Incrementals took 7 to 31 minutes, much longer than the first round. Time Machine copied at 4 to 30 MB/s. The 6-hour job limit was not hit.

Time Machine's own log says how much it copied per backup (medium run): 7.52 GB, then 2.09, 6.40, 3.26, 1.72 and 0.29 GB. Its "estimated full backup" of 177 to 282 GB in the log is the whole disk, not what it copies. **M**

### What changed before each incremental (medium run)

| Backup | Change | Files added | Files changed | Files deleted | Tree after |
|---|---|---:|---:|---:|---|
| inc1 | 10% of files edited in every group (4 KiB in small files, 1 MiB in large ones), 2 GiB added, 2% deleted | 2,640 | 3,646 | 1,559 | 38,322 files, 8.4 GiB |
| inc2 | 30% edited, 4 GiB added, 5% deleted | 4,130 | 11,497 | 3,371 | 40,330, 11.2 GiB |
| inc3 | 20% edited, 1 GiB added, 10% deleted | 8 | 8,065 | 4,034 | 36,304, 11.4 GiB |
| inc4 | 10% of photos and docs rewritten in full, 10% of the rest edited, 1 GiB added, 3% deleted | 1,024 | 3,627 | 1,121 | 36,207, 12.1 GiB |
| inc5 | 30% of notes and code edited, 10,000 notes added, 5% of notes deleted | 10,000 | 10,016 | 1,597 | 44,610, 12.1 GiB |

The large run's inc1 and inc2 made the same changes on the bigger tree: 8,540 and 25,880 files changed.

### Uploads per backup, 8 MiB chunks (C)

| Backup | Written (M) | 8 MiB upload | 1 MiB upload | 256 KiB upload | 8 MiB upload / written |
|---|---:|---:|---:|---:|---:|
| medium first | 7.9 GiB | 9.5 GiB | 8.2 GiB | 8.0 GiB | 1.2 |
| medium inc1 | 1.8 GiB | 5.2 GiB | 2.4 GiB | 2.0 GiB | 2.9 |
| medium inc2 | 4.0 GiB | 12.3 GiB | 5.5 GiB | 4.5 GiB | 3.1 |
| medium inc3 | 1.5 GiB | 12.9 GiB | 3.5 GiB | 2.2 GiB | 8.8 |
| medium inc4 | 1.6 GiB | 18.8 GiB | 4.7 GiB | 2.6 GiB | 11.6 |
| medium inc5 | 0.4 GiB | 12.5 GiB | 2.6 GiB | 1.1 GiB | 30.1 |
| large first | 16.8 GiB | 20.6 GiB | 17.5 GiB | 17.0 GiB | 1.2 |
| large inc1 | 2.1 GiB | 13.5 GiB | 4.0 GiB | 2.7 GiB | 6.5 |
| large inc2 | 4.9 GiB | 26.8 GiB | 8.6 GiB | 6.1 GiB | 5.5 |
| large inc3, partial | 1.7 GiB | 15.7 GiB | 4.3 GiB | 2.6 GiB | 9.1 |

"Written" is also what JuiceFS would upload. The worst case is many small files: medium inc5 wrote 427 MiB and would upload 12.5 GiB at 8 MiB.

### Trash, 8 MiB chunks (C)

Bytes sent to the trash per backup, and how much of it is hot chunks (10 or more versions in that backup):

| Backup | Sent to trash | from hot chunks | 1 MiB | 256 KiB |
|---|---:|---:|---:|---:|
| medium inc1 | 3.5 GiB | 62% | 0.8 GiB | 0.3 GiB |
| medium inc2 | 8.5 GiB | 83% | 1.6 GiB | 0.6 GiB |
| medium inc3 | 11.6 GiB | 88% | 2.2 GiB | 0.8 GiB |
| medium inc4 | 17.3 GiB | 92% | 3.2 GiB | 1.1 GiB |
| medium inc5 | 12.4 GiB | 77% | 2.4 GiB | 0.9 GiB |
| large inc1 | 11.5 GiB | 76% | 2.0 GiB | 0.7 GiB |
| large inc2 | 22.1 GiB | 89% | 3.9 GiB | 1.4 GiB |
| large inc3, partial | 14.1 GiB | 82% | 2.7 GiB | 1.0 GiB |

**Short wait** (the decision on #597: keep the last 4 copies, copies every 15 minutes, delete a replaced version once 5 newer copies have landed). Peak bytes held at one time, mean over 15 copy-grid offsets, min to max:

| Chunk size | Medium, as run | Medium, hourly | Large, hourly (first 2 incrementals and part of inc3) |
|---|---:|---:|---:|
| 8 MiB | 40.9 GiB | 26.4 GiB (24.2 to 28.3) | 29.2 GiB (28.5 to 29.7) |
| 1 MiB | 7.7 GiB | 4.9 GiB (4.4 to 5.2) | 5.4 GiB (5.3 to 5.5) |
| 256 KiB | 2.8 GiB | 1.7 GiB (1.5 to 1.9) | 1.9 GiB (1.8 to 2.0) |

- As run, backups were back to back, so the wait spanned two to four backups. Hourly is the same events with one backup per hour, or a minute after the last one if it ran over an hour. **C**
- **Extrapolated to hourly backups for 7 days (E):** the short-wait trash does not grow with time. It stays at one or two backups' worth. If every hour looked like the medium incrementals, that is about 21 GiB on average and 30 GiB at worst at 8 MiB, 4 to 6 GiB at 1 MiB, and 1.5 to 2 GiB at 256 KiB. For comparison, A would hold about 1.7 TiB and B-lite about 170 GiB at 8 MiB.
- Changing 10% to 30% of files every hour is far above a normal Mac. The first round's light incrementals gave 2.8 GiB on average. Real use lies between, closer to the first round. **I**

### Hot spots (C)

Versions per 8 MiB chunk, medium run, top 8:

| File | Offset (MiB) | first | inc1 | inc2 | inc3 | inc4 | inc5 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `bands/0` | 0 | 103 | 101 | 155 | 181 | 212 | 214 |
| `mapped/0` | 0 | 59 | 70 | 98 | 112 | 162 | 129 |
| `bands/0` | 112 | 57 | 70 | 98 | 112 | 162 | 129 |
| `bands/5` | 240 | 44 | 61 | 74 | 75 | 107 | 78 |
| `bands/0` | 120 | 0 | 23 | 65 | 90 | 143 | 109 |
| `bands/5` | 248 | 1 | 5 | 45 | 72 | 115 | 91 |
| `bands/6` | 32 | 3 | 14 | 29 | 53 | 72 | 45 |
| `bands/24` | 240 | 0 | 16 | 48 | 53 | 78 | 14 |

- The four spots from round one are hot in every backup again, now with 44 to 214 versions per backup. Their neighbours (`bands/0` at 120 MiB, `bands/5` at 248 MiB) join from inc1 on.
- Chunks with 10 or more versions: medium 15, 39, 42, 46 and 41 per incremental. Large: 54, 91, and 49 in the partial inc3. Round one had 1 to 13.
- Hot spots grow with the number of files changed, not bytes. Medium inc5 changed 289 MB of small files and has as many hot chunks as inc3.

### Peak unflushed data and the 256 MiB RAM budget (C)

Unflushed data: dirty chunks at object size, held until their file's FLUSH or CLOSE (`scratch.py`, as in section 6).

| Backup | Peak | p99 over time | Over 256 MiB: WRITEs | Uploaded early | Extra versions (MiB) |
|---|---:|---:|---:|---:|---:|
| medium first | 838 MiB | 819 MiB | 464 | 3.6 GiB | 34 (259) |
| medium inc1 | 527 MiB | 511 MiB | 56 | 448 MiB | 15 (120) |
| medium inc2 | 541 MiB | 439 MiB | 181 | 1.4 GiB | 33 (264) |
| medium inc3 | 842 MiB | 637 MiB | 99 | 791 MiB | 28 (223) |
| medium inc4 | 611 MiB | 420 MiB | 55 | 440 MiB | 2 (16) |
| medium inc5 | 190 MiB | 108 MiB | 0 | 0 | 0 |
| large first | 1,668 MiB | 1,636 MiB | 1,024 | 8.0 GiB | 90 (718) |
| large inc1 | 738 MiB | 563 MiB | 92 | 736 MiB | 10 (80) |
| large inc2 | 821 MiB | 546 MiB | 211 | 1.7 GiB | 36 (282) |
| large inc3, partial | 504 MiB | 240 MiB | 44 | 352 MiB | 10 (80) |

- The budget model: when a WRITE pushes the dirty total over 256 MiB, the least recently written chunks are uploaded early until it fits. An early chunk that is written again before its FLUSH costs one extra version. Sparse zero chunks change nothing here, because the peaks fall in data copy, not the erase.
- **The budget is exceeded in every backup except the small-file one.** The first backup uploads 46% to 48% of its bytes early. Incrementals upload 0.4 to 1.7 GiB early.
- **Early upload is cheap in versions.** Only 2 to 90 early chunks per backup were written again before their FLUSH, at most 0.7 GiB. That is under 4% of the upload of the same backup.
- The peak grew with the tree, 0.8 GiB with a 7 GiB tree and 1.7 GiB with a 15.7 GiB tree. It follows how long macOS writes before FLUSH. **I**

### Database rows at the end (C)

| Run | Image | 8 MiB rows | 1 MiB rows | 256 KiB rows | Files |
|---|---:|---:|---:|---:|---:|
| medium, after inc5 | 16.0 GiB | 2,122 | 16,480 | 65,708 | 135 |
| large, in inc3 | 24.3 GiB | 3,220 | 25,037 | 99,839 | 201 |

Per TiB of image (E): about 136,000, 1.05 million and 4.2 million rows, the same as round one.

### Summary of round two

- With 10% to 30% of files changed per backup, 8 MiB chunks upload about 3 to 30 times the bytes written, and send 3.5 to 22 GiB per backup to the trash.
- Short wait holds about 26 to 29 GiB at peak at 8 MiB with hourly backups like these, 5 GiB at 1 MiB, and under 2 GiB at 256 KiB. It does not grow with time.
- A 256 MiB RAM budget forces early uploads in almost every backup, but adds under 4% extra versions.

## Limits

- **Back to back, not hourly.** The 6 backups ran 1 to 2 minutes apart. The hourly numbers move the measured events onto an hourly grid. macOS might write a bit differently after an hour of idle time.
- **One run, one Mac, one tree.** The tree is synthetic random data. A real Mac has more files and system data, and probably more APFS metadata work per backup.
- **Little of the Mac itself.** The harness excludes most of the Mac. Real first backups are much larger. The hot spots look tied to APFS metadata work (file count and deletes), not to data size.
- **No thinning.** Time Machine deletes old backups when space runs out, or after 24 hours and a month. That was not exercised.
- **No encryption.** As in #528, the runner cannot turn it on.
- **Harness writes between backups** (810 MiB of 8 MiB versions sent to the trash) were left out. With them, the incremental trash at 8 MiB goes up by about 11%.
- **The model's write path** uploads every dirty chunk at each FLUSH. Holding chunks back, or merging FLUSHes, would change all numbers. Truncates that leave a partial chunk are counted as no upload, which is rare here.
- **The server under the trace** is the JuiceFS-backed `smbnext` from `research/tm-mac-trace`, not current `main`. The request pattern comes from macOS, so this should not matter. **I**

## Files

- `test/macos/hotspots_test.go`, and small changes in `scenario_test.go`, `harness_test.go`, `run.sh` and `.github/workflows/macos.yml`: the `hotspots` scenario and longer budgets.
- `research-tm-hotspots/hotspots.py`: replays the trace into chunk tables and writes JSON.
- `research-tm-hotspots/report.py`: prints the tables from that JSON.
- `research-tm-hotspots/scratch.py`: bytes waiting in local scratch until FLUSH, and early uploads under a RAM budget (sections 6 and 7).
- `research-tm-hotspots/out-medium.json`, `out-large.json`: the output for runs 37393029984 and 37380880797 (section 7). The large run needs the extra window `inc3-partial=2026-10-05T23:39:02.497165Z,2026-10-06T00:09:24Z`.
- `test/macos/hotspots_large_test.go` and a free-disk step in the workflow: the `hotspots-large` and `hotspots-medium` scenarios.
- `research-tm-hotspots/out.json`: the output for run 37361509487.
- Input: `application-1-initialize.log` and `mac-harness.log` from the `mac-hotspots-*` artifact of run 37361509487 (kept 14 days).
