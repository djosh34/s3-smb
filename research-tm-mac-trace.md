# What Time Machine sends to the new SMB server

Ticket: [#528](https://github.com/djosh34/s3-smb/issues/528). Follows [#510](https://github.com/djosh34/s3-smb/issues/510) and its report on `research/tm-smb-ops`. Status: done, 2026-10-05. Throwaway branch. Nothing here lands on `main`.

## Runs

| Run | Harness | Result |
|---|---|---|
| [37288480415](https://github.com/djosh34/s3-smb/actions/runs/37288480415) | as on `main`, plus logging (4f905aa) | Backup job failed before Time Machine started. Other jobs cancelled by me. |
| [37289256176](https://github.com/djosh34/s3-smb/actions/runs/37289256176) | plus one harness change (eefaea0) | [Backup job](https://github.com/djosh34/s3-smb/actions/runs/37289256176/job/111695526387) passed: full first backup, then the harness attached the image and found the backup. Scenario and recovery jobs cancelled by me, not analysed. |

Both runs: `server=smbnext`, `mode=acceptance`, `macos-15-intel`, macOS 15.7.9 (24G830). "Encrypt backups" was off. No encrypted run was made (see "Encryption" below).

### Run 1 failure

`tmutil setdestination` failed with "Failed to open session. (error 80)" and "The backup destination could not be set." The server log shows why: `login refused`, `another client is logged in or has durable opens`. The harness mounts the share with `mount_smbfs` first and keeps it mounted. tmutil then opens its own SMB session. The new server allows one client since #501, so it refused tmutil. The logging change was not involved. It ran and logged 57 requests. No Mac run of `smbnext` had happened since #501.

Run 2 adds one harness line on this branch: unmount the harness share before `setdestination`. The server is unchanged. With that, every session in the run was sequential: harness mount, tmutil, backupd, harness verify mount.

## Method

`internal/smb/server/trace.go` logs one JSON line (`"msg":"smb trace"`) per request, from `execute`, after the handler returns. Fields: command, status, handler time, file ID, path (from the CREATE name, updated on rename), stream name, CREATE disposition, options, access, action and size, info type and class, READ and WRITE offset and length, a per-write all-zero flag, the first 64 bytes of writes at offset 0 and bytes 512 to 527, FLUSH variant (`data` = Reserved1 0, `full` = 0xFFFF), rename target and ReplaceIfExists, EOF, allocation and delete-pending values, and the open-table handle count and distinct-inode count. `state.Table.TraceCounts` gives the counts. The lines go to the application log, which the harness saves in the `mac-backup-*` artifact.

Labels as in #510: **O** observed in run 2's trace, **I** inferred.

Run 2 had 9,263 requests in three phases. Times are UTC. Phase limits come from the harness log.

| Phase | Time | Requests | What runs |
|---|---|---|---|
| setdestination | 09:28:09 to 09:28:19 | 100 | harness mount and unmount, `tmutil setdestination` |
| backup | 09:28:19 to 09:29:13 | 5,209 | `tmutil startbackup --block`: backupd and DiskImages |
| verify | 09:29:13 to 09:30:02 | 3,954 | harness: mount, `hdiutil attach -readonly`, `tmutil listbackups` |

The backup timeline, from backupd's log and the trace:

1. 09:28:21. Bundle `<name> 2026-10-05-092821.incomplete` created. "Using a band size of 268.4 MB". Image resized to 16 TB.
2. 09:28:23. Attach. DiskImages probes `bands/0` and `bands/8d4` 26 times (all not found).
3. 09:28:34 to 09:28:47. "Erasing ... as Case-sensitive APFS". This is where 93% of all band bytes are written, all zeros (see Bands).
4. 09:28:53. Unmount. 09:28:55: bundle renamed to `<name>.sparsebundle` and attached again.
5. 09:29:02 to 09:29:06. Copy. 09:29:08: snapshot unmounted. 09:29:12: detach.

The tested tree is small, 4 MB of random data plus two small files. The Mac's own data is mostly excluded. Counts below are for this one small first backup.

## Facts

### Request counts per command (O)

| Command | All | Success | Backup phase |
|---|---:|---:|---:|
| READ | 3,755 | 3,755 | 0 |
| WRITE | 3,736 | 3,736 | 3,736 |
| CREATE | 439 | 399 | 352 |
| CLOSE | 399 | 399 | 315 |
| FLUSH | 396 | 396 | 391 |
| QUERY_INFO | 187 | 158 | 145 |
| QUERY_DIRECTORY | 172 | 106 | 132 |
| SET_INFO | 127 | 127 | 126 |
| TREE_CONNECT | 22 | 6 | 5 |
| SESSION_SETUP | 8 | 4 | 2 |
| TREE_DISCONNECT, NEGOTIATE, IOCTL, CHANGE_NOTIFY, LOGOFF | 6, 4, 4, 4, 4 | 6, 4, 0, 0, 4 | 1 each |
| LOCK, OPLOCK_BREAK, CANCEL, ECHO | 0 | 0 | 0 |

- All READs are in the verify phase, by the harness's `hdiutil attach` and `tmutil listbackups`. 3,684 are 4 KiB. backupd read nothing during the backup.
- The 4 IOCTLs are `0x140078` (FSCTL_SRV_REQUEST_RESUME_KEY, server-side copy), once per mount. All got NOT_SUPPORTED. The 4 CHANGE_NOTIFY also got NOT_SUPPORTED. The 16 failed TREE_CONNECTs are BAD_NETWORK_NAME. The 40 failed CREATEs are all name-not-found or name-invalid lookups.

### Request counts per info class (O)

| Request | Class | All | Success |
|---|---|---:|---:|
| QUERY_DIRECTORY | FileIdBothDirectoryInformation (37) | 172 | 106 |
| QUERY_INFO file | FileAllInformation (18) | 57 | 57 |
| QUERY_INFO file | FileStreamInformation (22) | 25 | 25 |
| QUERY_INFO filesystem | FileFsSizeInformation (3) | 72 | 72 |
| QUERY_INFO filesystem | FileFsAttributeInformation (5) | 4 | 4 |
| QUERY_INFO security | (any) | 29 | 0 (NOT_SUPPORTED) |
| SET_INFO file | FileEndOfFileInformation (20) | 103 | 103 |
| SET_INFO file | FileDispositionInformation (13) | 16 | 16 |
| SET_INFO file | FileRenameInformation (10) | 8 | 8 |

- No other class was used. No SET_INFO Basic, so no times or attributes were set. No Allocation. No EA classes.
- QUERY_DIRECTORY is always class 37. 60 of the 66 failures are single-name lookups that find nothing (NO_SUCH_FILE), for example the target name before a plist rename or Spotlight markers on the root. The other 6 are end-of-listing.
- Security queries happen on bands, plists and `token` during the backup. All were refused and the backup still passed.

### Named streams and xattrs (O)

Time Machine wrote **no** named stream and no xattr. Only two stream names appeared, and both are opens that failed:

| Stream | Object | Count | Result | Size |
|---|---|---:|---|---|
| `AFP_AfpInfo` | share root | 4, once per mount | OBJECT_NAME_INVALID | none read or written |
| `com.apple.quarantine` | `token` | 4 | OBJECT_NAME_NOT_FOUND | none read or written |

There were 25 FileStreamInformation queries (on bands, plists, `token`, `lock`, directories). No stream was opened after them.

### Renames, deletes, truncates and EOF changes (O)

- **Renames: 8.** All have ReplaceIfExists = 0.
  - 1 directory: `<name> 2026-10-05-092821.incomplete` to `<name>.sparsebundle`.
  - 7 files: `com.apple.TimeMachine.MachineID.plist.tmp` (3), `SnapshotHistory.plist.tmp` (3), `Results.plist.tmp` (1), each to the name without `.tmp`. When the target existed, it was deleted first (disposition), then renamed.
  - No band, `mapped/` or `Info.*` file was renamed.
- **Deletes: 16, all by SET_INFO disposition.** No delete-on-close. 2 `.com.apple.timemachine.supported-<uuid>` probes, 2 `MachineID.plist`, 2 `SnapshotHistory.plist`, and **10 band-related**:
  - `bands/746a` and `mapped/746a`, twice; `bands/e8d4` and `mapped/e8d4`, twice; `bands/0` once. All during the APFS erase, each 0.2 to 4 s after the band was filled with zeros.
  - `bands/746a` and `bands/e8d4` were recreated and deleted again. They are not in the final image.
- **Truncates.** On bands: none. No band ever got a smaller EOF.
  - `Info.plist` and `Info.bckup` are rewritten in place 3 times each: EOF 0, EOF new size (562 or 575), one write at offset 0. They are not renamed. This answers #510 unknown 7.
- **EOF changes on bands: 73 SET_INFO EOF.**
  - Every one set the EOF to the size a write had just reached. None grew or shrank the file.
  - 1,017 writes extended a band past its EOF. So bands grow by writes, and the SET_INFO is only a confirmation.
  - This matches #510's note that macOS sends EOF after writes past EOF when AAPL caps are 0. **I**
- **CREATE dispositions.**
  - Bands are created with FILE_OPEN_IF (3), action CREATED.
  - The `.tmp` plists, `Info.*`, `token` and `lock` are created with FILE_OVERWRITE_IF (5).
  - No CREATE ever overwrote or superseded an existing band.

### Bands: sizes, writes and 8 MiB positions (O)

Band size is 256 MiB, so a full band has 32 positions of 8 MiB. Bands touched: `0`, `5`, `6`, `180`, `746a`, `e8d4`. `0x180` is 96 GiB into the image. `746a` and `e8d4` are near the middle and the end of the 16 TB disk. Those two were deleted.

Writes to bands: 3,648, 971 MB.

- **3,187 writes (904.7 MB, 93% of bytes) were all zeros.** The trace sees this because it checks every write.
- Zero writes are 256 KiB, 512 KiB or 1 MiB. They run in ascending order from offset 0, or 4096, to a fixed end, and that end sets the band's size. They come during the APFS erase.
- Nonzero writes: 461, 66.6 MB. Sizes: 256 KiB (238), 4 KiB (120), 16 KiB (35), 12 KiB (33), and a few others between 24 and 140 KiB.
- Every band write offset is 4 KiB aligned. No band write had WRITE_THROUGH, and no CREATE asked for it.

Final state of each band file, using its last incarnation:

| Band | Final size | Positions in file | Never written (of 32) | Only zeros | Zeros and data | Only data | Nonzero bytes |
|---|---:|---:|---:|---:|---:|---:|---:|
| `0` | 124,817,408 (119 MiB) | 15 | 17 (all past EOF) | 13 | 2 (pos 0, 14) | 0 | 6.1 MB |
| `5` | 268,435,456 (256 MiB) | 32 | 0 | 30 | 1 (pos 30) | 1 (pos 31) | 13.8 MB |
| `6` | 46,530,560 (44.4 MiB) | 6 | 26 (all past EOF) | 0 | 0 | 6 | 46.5 MB |
| `180` | 7,798,784 (7.4 MiB) | 1 | 31 (all past EOF) | 0 | 1 | 0 | 0.2 MB |
| `746a` (deleted) | 86,573,056 before delete | 11 | 21 | 11 | 0 | 0 | 0 |
| `e8d4` (deleted) | 173,080,576 before delete | 21 | 11 | 21 | 0 | 0 | 0 |

- Inside each band file, every 8 MiB position was written at least once. The "never written" positions are all past the file's end. Band files end at the highest byte written. They are not filled to 256 MiB, which corrects #510's guess that touched bands are filled to full length.
- Of the 4 live bands, 11 of 54 in-file positions carry nonzero data. 43 hold only zeros.

### How scattered writes are within 8 MiB chunks (O)

All 11 positions that hold data were written as one gapless extent, from their lowest to their highest written byte. No holes inside a written range. Two patterns:

- **Bulk data**: band `6` positions 0 to 5, band `5` position 31.
  - About 32 writes of 256 KiB each, so one pass over 8 MiB.
  - Mostly ascending. On band `6` positions 0 to 4, 25 to 31 of the next writes were sequential and 0 to 2 went backward. Band `5` position 31: 16 sequential, 5 backward.
  - No 4 KiB block was written twice. Interleaved with other bands. Done within 10 s.
- **APFS metadata**: band `0` positions 0 and 14, band `5` position 30.
  - 62 to 102 writes, mostly 4 to 40 KiB, spread over the whole 28 s from erase to detach.
  - Many backward jumps: 37, 29 and 19.
  - The same 4 KiB blocks were rewritten 68, 11 and 46 times.
  - Band `0` position 0 gets 4 KiB writes at rising offsets (the APFS checkpoint area, **I**) and 6 rewrites of block 0, the container superblock. Each burst is followed by a `data` FLUSH.

### FLUSH (O)

396 FLUSHes: 291 `data` and 105 `full`. Per file:

| File | data / full | First to last | Median gap | Most bytes between two FLUSHes (all / nonzero) |
|---|---|---|---|---|
| `Info.plist` | 80 / 80 | 09:28:22 to 09:29:12 | 0.01 s | 575 / 575 |
| `bands/0` | 71 / 3 | 09:28:36 to 09:29:18 | 0.06 s | 123.5 MB / 4.8 MB |
| `mapped/0` | 34 / 0 | 09:28:36 to 09:29:12 | 0.15 s | 8 KiB |
| `bands/5` | 26 / 0 | 09:28:49 to 09:29:12 | 0.12 s | 254.9 MB / 12.0 MB |
| `mapped/5` | 25 / 0 | | 0.12 s | 8 KiB |
| `bands/6` | 9 / 2 | 09:29:03 to 09:30:02 | 2.1 s | 40.1 MB / 40.1 MB |
| `mapped/6` | 9 / 0 | | 0.9 s | 8 KiB |
| `bands/746a`, `bands/e8d4` | 2 / 0 each | erase only | | 86.6 MB, 173.1 MB, all zeros |
| `bands/180`, `mapped/180` | 1 / 0 each | | | 7.8 MB / 0.2 MB |
| plists `.tmp`, final plists, `Info.bckup`, `token` | 2 to 6 / 1 to 4 each | | | under 1.2 KB |

- **Most bytes written to one file between two FLUSHes:**
  - 254,861,312 bytes (243 MiB) on `bands/5`, all zeros, during the erase.
  - For nonzero data: 40,124,416 bytes (38 MiB) on `bands/6`, during the copy.
  - Every write was followed by a FLUSH of the same file before the trace ended.
- **Band FLUSHes are almost all `data`.** The `full` FLUSHes go to `Info.plist`, in pairs. 80 of the 105 `full` are on `Info.plist`, up to 13 per second. Each comes right after a `data` FLUSH on the same handle. That is smbfs's F_FULLFSYNC (#510). **O**
- The 5 `full` FLUSHes on bands come after detach, at 09:29:18 and later. The harness's attach and listbackups made them, not backupd.
- The order in each round is: band writes, a `data` FLUSH on each written band and `mapped/` file, then `data` + `full` on `Info.plist`. All 80 `full` FLUSHes on `Info.plist` follow a `data` FLUSH on the same handle. **O** So DiskImages uses F_FULLFSYNC on `Info.plist` as its barrier, because "barriers not supported" on smbfs. **I**
- A server that treats a `full` FLUSH as a flush of only that handle would make only `Info.plist` durable at the barrier. The band data before it was already covered by the band's own `data` FLUSH. **I**
- Median server time per FLUSH: 6.1 ms on bands (`data`) and 2.4 ms for `full`. The longest was 490 ms.
- The gap between two FLUSHes of one file has a median of 0.06 s, a p90 of 3.0 s and a maximum of 33.5 s.

### Open handles (O)

- **Peak: 15 open handles on 15 distinct files.** It was at 09:28:45, during the erase. The 15 were:
  - 5 bands and their 5 `mapped/` files
  - `Info.plist`, `Info.bckup`, `token`, `lock`
  - the share root
- At most 5 band handles were open at once.
- Handles never exceeded distinct files by more than 1. So one file is almost never open twice.
- Peak in the verify phase: 6. All handles were closed when the backup ended at 09:29:12. The 100 deferred-close handles from #510 did not show up.
- No LOCK requests at all. The bundle's `lock` file is opened 7 times, queried and closed, never locked or written.

### Other files

- `mapped/<band>` is one 8,192-byte file per band. It is rewritten in place at offset 0 after band writes, 73 times in total. 8 KiB is 65,536 bits, one per 4 KiB block of a 256 MiB band, so it is a block bitmap. **I**
  - `mapped/0` starts `ff..`. Band `0` has data at its start.
  - `mapped/5` starts `00..` even though band `5` was zero-filled from offset 0.
  - So zero-filled blocks are marked unmapped. **I**
- The zero fills, the unmapped bits and the deletes of all-zero bands fit one reading. During the APFS erase, macOS unmaps (TRIMs) the disk. On a share without hole punching, DiskImages writes zeros over the band range, clears the bits, and deletes a band once it is all unmapped. **I** The trace shows the writes and deletes. It cannot show the TRIM itself.
- `token` was opened 13 times and FLUSHed (2 data, 2 full). It was never written and stayed at size 0.

## Encryption

Not tested. tmutil on the runner offers no way to turn on "Encrypt backups". Its usage, saved by the harness in run 2, is:

```
Usage: tmutil setdestination [-a]  mount_point
       tmutil setdestination [-ap] afp://user[:pass]@host/share
```

`-p` only prompts for the share password. The man page on macOS 15.7.9 has no encryption option. After `setdestination`, the destination entry in `/Library/Preferences/com.apple.TimeMachine.plist` has no encryption key either. Encryption is set in System Settings, which a headless runner cannot drive without UI scripting and privacy grants. Pre-creating an encrypted sparsebundle with `hdiutil` would test a different path from what backupd does itself. So the second run was skipped.

### `token` and band `0` without encryption (O)

- backupd logged "Creating an unencrypted sparsebundle diskimage".
- `token`: size 0, never written.
- Band `0`, first nonzero write at offset 0 (09:28:45):
  - bytes 0 to 31: `7c32c2b9948b6570 0100000000000000 0100000000000000 0100008000000000`
  - bytes 32 to 35: `NXSB`
  - bytes 512 to 527: all zero
- That is an APFS container superblock (object ID 1, type 0x80000001) at image offset 0. **There is no GPT.** #510 expected `EFI PART` at offset 512. That is wrong for macOS 15 Time Machine images: the APFS container starts directly at image offset 0.
- All 6 nonzero writes at offset 0 keep `NXSB`. The transaction ID goes 1, 1, 2, 5, 6, 32.
- With encryption on, the comparison would be: does `token` start with `encrcdsa`, and is offset 32 of band `0` still `NXSB`? Not measured.

## Unknowns

1. What "Encrypt backups" changes: the `token` header, band `0` bytes, and the request pattern. Needs a run with encryption turned on through System Settings, or another way to set it.
2. Incremental backups, thinning and band deletes outside the erase. This run made one small first backup.
3. Behaviour with more data. Only one band (`6`) received bulk copy data here.
4. Whether DiskImages deletes all-zero bands on every unmap, or only during the erase. **I** above.
5. Whether `Info.plist`'s `full` FLUSH must also make earlier writes on other handles durable. Apple's F_FULLFSYNC meaning suggests yes. The trace cannot tell.

## Files

- `internal/smb/server/trace.go`, `internal/smb/state/trace.go`, two lines in `async.go`: the temporary logging.
- `test/macos/scenario_test.go`: unmount before `setdestination`, save tmutil usage and the TM preferences.
- `research-tm-mac-trace/`: the analysis scripts. Input: `application-1-initialize.log` from the `mac-backup-*` artifact of run 37289256176.
