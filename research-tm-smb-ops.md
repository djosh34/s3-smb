# What Time Machine does on an SMB share

Ticket: [#510](https://github.com/djosh34/s3-smb/issues/510). Status: done, 2026-10-05. Read-only research. No code changed.

The design points from [#173](https://github.com/djosh34/s3-smb/issues/173) that this report checks against: no database, fixed-name 4 MiB chunk objects `<path>/<index>` with no manifest, size from the end of the last chunk, modified time from S3 Last-Modified, no local state, no encryption, no compression, Time Machine only.

## Labels and sources

Each fact has one label:

- **O** observed, with the source.
- **D** documented, with a link or a file and line in Apple's source.
- **I** inferred. The reasoning is given.

Sources:

- **[CI]** Mac CI run [37198931935](https://github.com/djosh34/s3-smb/actions/runs/37198931935) on main at dd6b76f. macOS 15.7.9 (24G830), the old server (no `smbnext` tag), JuiceFS storage. Artifact `mac-backup-*`, file `0130-log.log` (unified log, backupd and DiskImages lines). Artifact `mac-server-kill-restart-*`, file `0197-log.log` (a second mount and backup of the same bundle).
- **[A]** Apple SMBClient-494.120.2 (macOS 15.6 smbfs), the tree used for research-macos-client.md. Paths are relative to `kernel/`.
- **[N1]** `research-macos-client.md` (issue [#165](https://github.com/djosh34/s3-smb/issues/165)). **[N2]** `research-storage-engine.md` (issue [#168](https://github.com/djosh34/s3-smb/issues/168)).
- **[PR63]** [PR #63](https://github.com/djosh34/s3-smb/pull/63), band size against reported share size.
- **[R]** This repo at 811f034.
- **[TMS]** [Apple, Time Machine over SMB specification](https://developer.apple.com/library/archive/releasenotes/NetworkingInternetWeb/Time_Machine_SMB_Spec/).
- **[W1]** [Eclectic Light, Time Machine to APFS: using a network share (2021)](https://eclecticlight.co/2021/04/16/time-machine-to-apfs-using-a-network-share/).
- **[W2]** [Crucial Security, Crack and image a FileVault sparsebundle (2011)](https://crucialsecurity.wordpress.com/2011/03/30/4/), on the `token` file.

Not available: a per-request SMB trace of a Time Machine backup. The server logs no requests. The issue #64 packet captures were encrypted and never parsed. So the SMB-level counts below (FLUSH rate, open handles, SET_INFO calls) come from Apple's client source and from backupd log lines, not from a wire trace.

## 1. Sequence of a first backup

From [CI], `mac-backup`, timestamps 11:37:28 to 11:38:32 UTC. All lines are **O**.

1. Mount `smb://…/TimeMachine`. backupd sets network volume options (reconnect timeout 30 s, QoS 0x20). On the old server this fsctl fails with error 45 (ENOTSUP), and the backup still goes on.
2. "Creating an unencrypted sparsebundle diskimage at …/`<host>-<id> 2026-10-04-113730.incomplete`".
3. "Using a band size of 268.4 MB (on a volume with size of 1.1 TB)".
4. `Info.bckup` written ("disk size: 0"), then `Info.plist`.
5. "Looking for existing bands … Found 0 existing bands". This is a listing of `bands/`.
6. `Info.bckup` and `Info.plist` written again ("disk size: 16000000000000"). "Disk image resized to 16000000000000 bytes". The virtual disk is 16 TB.
7. Attach. DiskImages logs "File system is smbfs … remote mount, barriers not supported".
8. "Erasing 'Backups of …' as Case-sensitive APFS". This took 22 s.
9. `com.apple.TimeMachine.MachineID.plist.tmp`, then `com.apple.TimeMachine.MachineID.plist`, each with F_FULLFSYNC. This pair happens 3 times.
10. Detach. F_FULLFSYNC on `Info.plist` and `token`.
11. "Renamed '…`.incomplete`' to '…`.sparsebundle`'". F_FULLFSYNC on `Info.plist` and `token` again, under the new name.
12. Attach again, `fsck_apfs -q -n`, mount the APFS volume.
13. `com.apple.TimeMachine.SnapshotHistory.plist.tmp` then `.plist`, with F_FULLFSYNC.
14. Copy 4.1 MB. F_FULLFSYNC of the inner APFS volume, APFS snapshot, another F_FULLFSYNC.
15. `SnapshotHistory.plist` safe-save twice more, then `com.apple.TimeMachine.Results.plist.tmp` and `.plist`.
16. "Backup succeeded". Unmount.

A later mount of the same bundle ([CI], `mac-server-kill-restart`, 12:02:21) lists `bands/` again on attach, rewrites one `com.apple.TimeMachine.*` plist pair before the backup, and does the `SnapshotHistory` safe-save. **O**

## 2. Files on the share

| Path | What | Label |
|---|---|---|
| `<name> <date>.incomplete/` | The bundle while it is being created. Renamed once. | O [CI] |
| `<name>.sparsebundle/` | The bundle. `<name>` is the Mac's computer name plus an id. | O [CI] |
| `Info.plist`, `Info.bckup` | Image header and its copy. Keys include `band-size` and `size`. Written twice at creation, F_FULLFSYNC on `Info.plist`. Small (under 1 KiB). | O [CI] for writes; D for keys ([gist](https://gist.github.com/sansumbrella/4012632)); size I |
| `token` | Empty for an unencrypted image. F_FULLFSYNC before and after the rename. | O [CI] for the fsync; D [W2] for empty |
| `bands/<hex>` | Band files. Name is the band index in lowercase hex, no padding. | D [W1]; naming O in [R] `test/e2e/namespace_measurement_test.go` |
| `mapped/` | One file per band, in a folder named `mapped`. Purpose not documented. | D [W1]; not checked in our runs |
| `com.apple.TimeMachine.MachineID.plist` | Machine identity. Written at creation and at the start of later backups. | O [CI] |
| `com.apple.TimeMachine.SnapshotHistory.plist` | List of backups. Rewritten 2 to 3 times per backup. | O [CI] |
| `com.apple.TimeMachine.Results.plist` | Last result. Rewritten once per backup. | O [CI] |
| `*.plist.tmp` | Temporary for each plist write. | O [CI] |
| `.com.apple.timemachine.supported-<uuid>` | Durable handle probe: CREATE, CLOSE, delete, at each kWriteSettings. Only when the server advertises LEASING. | D [N1], [A] `smbfs_smb_2.c:6953-7110` |

Not seen in any log: a lock file, `.DS_Store` or `._` files on the share. The Time Machine mount is hidden under `/Volumes/.timemachine`, so Finder does not browse it. **I**

### Band size and count

- Band size depends on the share size. 1 TiB reported gives 268.4 MB (256 MiB). **O** [CI]. 1.13 PB gives 8.59 GB (8 GiB). **O** [PR63]. Apple smbd at 348 GB gave 85.1 MB. **O** [PR63]. [W1] saw 67.1 MB (64 MiB). **D**
- The values fit "share size / 4096, rounded to a power of two MiB, capped at 8 GiB". **I** from 4 data points.
- The band size is fixed for the life of the bundle. It sits in `Info.plist` and every offset depends on it. **I**
- The virtual disk is 16 TB whatever the share size. **O** [CI]. At 256 MiB bands that is at most 59,605 bands. Each band is 64 chunks of 4 MiB. At 8 GiB bands it is 2,048 chunks per band.
- Count in practice: about 4 bands per GB of backup at 256 MiB bands. **I**, matches [N2].
- "Initialized bands array of size 64" at every open. Probably the number of band handles DiskImages keeps. **O** for the line, **I** for the meaning.

## 3. Times and attributes

### When the client sends SET_INFO FileBasicInformation

From [A]. **D** for each line.

- `smbfs_vnop_setattr` sends it only when a program sets times or flags: utimes, setattrlist, chflags (`smbfs_vnops.c:6172-6390`).
- On create, smbfs drops any times the caller passes: "The server will set all times for us" (`smbfs_set_create_vap`, `smbfs_vnops.c:7614-7660`).
- Writing an xattr on SMB 2/3 sends no time reset (the reset at `smbfs_vnops.c:12134` is SMB1 only). Removing FinderInfo does send one (`:12515`), to set the old modify time back.
- Server-side copy sets mtime on the target (`smbfs_smb_2.c:7616`). Time Machine does not use server-side copy. **D** [N1].

Whether backupd or DiskImages call utimes or chflags on any file in the bundle is **unknown**. Nothing in the logs shows it. Band files are written through pwrite and F_FULLFSYNC, and plists through write-temp-then-rename. Neither needs a time set. **I**

### What the client does with times

- The client keeps mtime and size from the last CLOSE. On the next open it compares them with the server's values. If either changed, it drops its page cache for that file. `smbfs_vnops.c:1583-1600, 3040-3065`. **D**
  - So mtime must not move while a file is closed and unchanged. If it does, the only cost is a cache drop. **I**
  - With one client, a stale mtime cannot cause wrong data, because the client's own writes are in its cache. **I**
- CLOSE replies with zero times make the client log "Bad SMB 2/3 Server, close attr of ChangeTime is 0" and fetch the attributes again with a QUERY_INFO. The old server does this: 89 lines in one backup, 199 in the kill scenario. **O** [CI]. Each one costs an extra round trip. **I**
- DiskImages, backupd and APFS keep their own times inside the image. Time Machine's backup dates come from APFS snapshots and `SnapshotHistory.plist`, not from file times on the share. **I**

### Can S3 Last-Modified be the modified time?

Probably yes, with these limits. **I** throughout.

- A file's mtime is the newest Last-Modified among its chunks. A write into chunk 3 of a band does not touch the last chunk, so the last chunk alone is not enough.
- So a QUERY_DIRECTORY entry needs a LIST of all chunks of that file. For `bands/` that is a LIST of the whole prefix: about 239 pages of 1,000 keys per TB stored at 4 MiB chunks. DiskImages lists `bands/` at every attach (**O** [CI]), and directory entries carry size and times, so the server must produce them for every band.
- One-second precision is fine. The client rounds to 100 ns and only checks for equality.
- Last-Modified changes when a chunk is rewritten, even with the same bytes. That only causes a cache drop.
- Creation time and change time have no S3 source. Reporting mtime for both is the simple choice. Nothing observed relies on them.
- Directories have no object. Their times can be a constant or the newest child. Nothing observed relies on them.
- On AWS, a multipart object's Last-Modified may be the time the upload started, not when it finished. This is not checked for B2 or Garage.

## 4. Size changes

- **Extending writes.** Bands grow by writes past the end. The new server reports AAPL server caps 0 (`internal/smb/features.go:43`). The client then treats it as a non-Unix server (`SMB_CAP_UNIX` is set only from `kAAPL_UNIX_BASED`, `netsmb/smb_smb_2.c:4880-4883`). For a non-Unix server, every write past EOF marks the file, and a SET_INFO EndOfFile follows at fsync or close (`smbfs_vnops.c:7525-7545`, `smbfs_smb.c:3026-3031`). **D**. The old server sent UNIX_BASED, so this did not happen there. **D** [N1].
- **ftruncate to a larger size.** On a non-Unix server the client writes one zero byte at `new_size - 1`, then sends SET_INFO EndOfFile (`smbfs_io.c:526-560`, `smbfs_vnops.c:5640-5700`). On a Unix server it sends only the SET_INFO, as a CREATE+SET_INFO+CLOSE compound. **D**
- **ftruncate to a smaller size.** SET_INFO EndOfFile, usually as a compound. **D**
- **F_PREALLOCATE** becomes SET_INFO FileAllocationInformation, rounded to the block size (`smbfs_vnops.c:13722-13795`). Whether DiskImages uses it is unknown.
- **Whether DiskImages truncates or extends bands.** Unknown. No log line shows it.
- **Plists.** Written to `.tmp` and renamed (section 5), so they are never truncated in place. **I**. Whether `Info.plist` and `Info.bckup` are rewritten in place (truncate and write) or by rename is unknown.

### Are bands sparse?

- [W1] reports that on an APFS host share "none of the band files … are themselves sparse files". **D**
- Formatting the image stored about 2.2 band sizes: 19 GB with 8.59 GB bands, and 197 MB with 85.1 MB bands. **O** [PR63]. The first small backup at 256 MiB bands stores 1.3 to 1.8 GB. **O** [R] README.
- Together this suggests that a touched band is usually written to its full length, often with zeros. **I**
- Holes inside a band are possible. APFS writes at any offset, and a pwrite past EOF leaves a gap. Whether DiskImages fills that gap is unknown. **I**
- A band shorter than the band size is possible. [W1] says "no more than", and bands grow by writes. **I**

### What this means for "size from where the last chunk ends"

**I** throughout.

- A trailing hole cannot exist in this layout. Any SET_INFO EndOfFile that extends a file must write a last chunk to fix the new size.
- An all-zero last chunk must not be skipped. It carries the size.
- Advertising `kAAPL_UNIX_BASED`, as Samba always does, turns off the client's deferred SET EOF and one-byte zero writes. It also changes other client paths (`UNIX_SERVER` checks in `smbfs_vnops.c:6017`, `smbfs_node.c:2098`, `smbfs_vfsops.c:1358, 1596`), so it needs a Mac run before it is relied on.

## 5. Renames

| Rename | When | How often | Label |
|---|---|---|---|
| Bundle directory `<name> <date>.incomplete` to `<name>.sparsebundle` | Creating a new backup | Once per destination. The directory holds `bands/` and the plists at that point. | O [CI] (both runs) |
| `com.apple.TimeMachine.*.plist.tmp` to `.plist` | Every plist write | MachineID: 3 at creation, about 1 per backup start. SnapshotHistory: 2 to 3 per backup. Results: 1 per backup. | O for the tmp and final fsyncs [CI]; rename is I |
| Silly rename to `.smbdelete<hex id>` | Deleting a file that is still open | Unknown. | D [A] `smbfs_smb.c:2580-2600` |

- **Rename with replace.** smbfs never sends ReplaceIfExists=1. It sets `replace_if_exists = 0` (`smbfs_smb_2.c:9920`). If the target exists, it first deletes it (or removes the directory), then renames (`smbfs_vnops.c:8660-8710`). **D**
  - So the server sees a delete then a plain rename, as two separate requests. A crash in between leaves only the `.tmp` file. **I**
- **Directory rename.** Only once per bundle, and only at creation, observed. If the server takes long, the risk is a timeout during the first backup. It cannot corrupt a running backup. **I**
- Time Machine never renames or moves band files. Their names are their positions. **I**

## 6. Deletes

- `.com.apple.timemachine.supported-<uuid>` is deleted after each probe. **D** [A]
- Each plist safe-save deletes the old plist (see the rename table). **D**/**I**
- A failed or abandoned first backup leaves a `.incomplete` bundle. Whether backupd deletes it on the next try or makes a new one is unknown. [N2] says the whole bundle is deleted after a failed first backup. That is not confirmed.
- **Thinning.** In APFS backups, thinning deletes APFS snapshots inside the image (**O** [CI]: "Starting age based thinning", "preserving sole backup"). That frees blocks inside the image, not files on the share. **I**
  - Whether DiskImages then deletes band files, empties them, or writes to `mapped/` is unknown. No delete of a band was seen. The test backups are too small to thin.
  - Rough scale if it does delete: removing 100 GB of old data at 256 MiB bands would be about 400 band deletes, each with up to 64 chunk objects. **I**

## 7. Streams and xattrs

- The client maps every xattr to a named stream when the share reports FILE_NAMED_STREAMS. FinderInfo becomes `AFP_AfpInfo` and the resource fork becomes `AFP_Resource`. **D** [N1]
- Both the old and the new server advertise named streams (`internal/app/server_old.go:44`, `internal/smb/features.go`). So no run has shown Time Machine working without them. **O** [R]
- Which xattrs backupd or DiskImages write on the bundle is **unknown**. Nothing in the logs names one. Candidates are FinderInfo (the bundle bit on the directory) and `com.apple.*` values. **I**
- Size: FinderInfo is 32 bytes. `AFP_AfpInfo` is 60 bytes. **D**. Other values are expected to be small. The new server caps streams at 64 KiB (`docs/smb-design.md`). **O** [R]
- [TMS] does not list streams as required. Every known working Samba Time Machine setup uses `streams_xattr`. **D** [N1]
- If streams are dropped, the client stores xattrs in AppleDouble `._` files, which are more files to rename and delete. **D** [N1]
- With AAPL, a FILE_OPEN of a 0-byte stream must return OBJECT_NAME_NOT_FOUND. **D** [N1]

## 8. File IDs

- smbfs assumes the server has file IDs and uses the ID as the vnode hash key (`smbfs_node.c:532-560`). **D**
- A file ID of 0 on any entry other than `..` turns file IDs off for the whole session. smbfs then hashes names instead (`smbfs_subr_2.c:1569-1602`). **D**
- If a known node comes back with a different ID, smbfs treats it as a different file (`smbfs_node.c:785-800`). **D**
- **Observed.** During creation, DiskImages saw the bundle directory with inode -1034610798094427774 before the rename and 1388083458052736697 after it. In the kill scenario it was 5897718715032486123, then -6995807582096967703. On a later mount, the same bundle showed inode 4, a JuiceFS inode. **O** [CI]
  - So an ID that changed across the one directory rename did not break the backup. **I**: the large values look like client-side values for newly created nodes ("Creates do not return the node id", `smbfs_node.c:788`).
- **Stability across server restarts.** Durable reconnect reopens handles with DH2C, which uses the SMB FileId (persistent and volatile handle), not the file ID. **D** [N1]. After reconnect, a QUERY_INFO that returns a different file ID for an open file makes the client treat it as a different file. **D**. So file IDs must be stable across a server restart for files that stay open. **I**
- **Without an inode table.** A non-zero 64-bit hash of the path meets both needs. It is stable across restarts and changes only on rename. Renames seen are of closed files: the bundle directory once, and closed `.tmp` plists. **I**. Whether a renamed file is ever still open on the Mac is unknown.

## 9. FLUSH, writes and open files

- **What a FLUSH is.** F_FULLFSYNC on a file sends a normal FLUSH, then a FLUSH with Reserved1=0xFFFF, on that file only (`smbfs_vnops.c:10440-10483`). fsync sends one normal FLUSH. **D**. FLUSH is per file, never "all open files". **D**
- **Who sends it.** backupd uses F_FULLFSYNC for every plist write and for the inner APFS volume at snapshot points (**O** [CI]). DiskImages reports "barriers not supported" on smbfs (**O** [CI]), so APFS barriers inside the image become F_FULLFSYNC on band files. **I**
- **How often.** The first backup logged 18 F_FULLFSYNC calls by backupd on share files (plists, `Info.plist`, `token`) and 2 on the inner APFS volume (**O** [CI]). Band files: unknown, DiskImages does not log them. Each inner-volume F_FULLFSYNC likely flushes every dirty band. **I**
- **Unflushed data.** Unknown. It is bounded by the bytes DiskImages writes between two APFS barriers. With a write lease the client also caches writes and pushes them at fsync or close. **I**
  - Earlier estimate in [N2]: every few seconds to tens of seconds while writing. Not measured.
- **Write sizes.** 1 MiB, 512 KiB and 256 KiB requests: 46%, 46% and 8% of bytes, 242 WRITEs in one old capture. **O** [N2] (workload-match).
- **Open files.** With leasing, smbfs keeps up to 100 closed files open as "deferred close" (`smbfs.h:813`, `smbfs_vfsops.c:1258`). **D**. DiskImages keeps a 64-entry band array (**O** for the line). So expect up to roughly 100 to 170 server handles at once. **I**
- **Directory listing.** DiskImages lists `bands/` at every create and attach (**O** [CI]). The client uses FileIdBothDirectoryInformation (class 37) and fills a cache with up to 10 async compound queries (**D** [N1]). Each entry needs size, times and file ID.

## 10. Encryption detection

- backupd logs "Creating an unencrypted sparsebundle diskimage". That wording suggests an encrypted variant is created as a disk-image-level encrypted bundle. **O** for the line, **I** for the meaning.
- For hdiutil-encrypted bundles, `token` holds the encryption header and starts with the ASCII bytes `encrcdsa`. Bytes 26 and 27 hold the key size. For an unencrypted bundle `token` is empty. **D** [W2]
- So a server-side check is possible from files alone: `token` with size 0 means not encrypted at the image level. `token` starting with `encrcdsa` means encrypted. **I**
- A second check: the band file `0` of an unencrypted image starts with a GUID partition table, so bytes 512 to 519 read `EFI PART`. In an encrypted image they are ciphertext. **I**
- `Info.plist` is not expected to differ. **I**
- If macOS 15 instead encrypts the APFS volume inside a plain image, `token` stays empty and the GPT is visible. The only signal is then the APFS volume superblock flag `APFS_FS_UNENCRYPTED`, which needs an APFS walk. **I**. Which of the two macOS 15 does is **unknown**. A Mac CI run with "Encrypt backups" on would settle it.

## 11. Other clients

- A Finder or smbclient browse needs only QUERY_DIRECTORY, QUERY_INFO, CREATE with read access, READ and CLOSE. **I**
- Finder on a writable share may try to write `.DS_Store` and `._` files. Refusing those writes is safe. **I**
- No other client was tested.

## Unknowns

1. Whether backupd or DiskImages ever set times or flags on any file in the bundle (SET_INFO FileBasicInformation).
2. Which xattrs or streams are written on the bundle or its files, and whether a backup works without streams.
3. Whether DiskImages truncates, extends or preallocates band files, and whether it zero-fills gaps.
4. Whether band files are ever deleted during thinning, and what `mapped/` holds.
5. FLUSH rate on band files and bytes written between FLUSHes, per file and in total.
6. Peak number of open handles during a backup.
7. Whether `Info.plist` and `Info.bckup` are rewritten in place or by rename.
8. What backupd does with a leftover `.incomplete` bundle.
9. Whether Time Machine's "Encrypt backups" on macOS 15 encrypts the disk image (`token` header) or the APFS volume inside it.
10. Whether advertising `kAAPL_UNIX_BASED` on the new server changes anything else that matters.
11. S3 Last-Modified for multipart objects on B2 and Garage: start or end of the upload.

Most of 1 to 7 can be measured in one Mac CI run on `smbnext`. The server would need to count requests by command and information class, and log stream names, SET_INFO classes, FLUSH timing with dirty bytes, and open-handle peaks. A second run with "Encrypt backups" on, followed by `xxd token` and a read of band `0`, settles 9.
