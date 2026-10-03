# SMB issue triage (s3-smb, main @ac712a6)

Severity is for macOS Time Machine (TM). critical: can corrupt or lose backup data, or crash the daemon. high: can fail a backup. low: TM never triggers it. Spot-checked in code: feature.go caps; CREATE oplock, DH2Q and RqLs handling; AAPL reply; FsAttribute flags; closeHandle/closeOpen; CHANGE_NOTIFY; READ async path; FullSize encoder; credit code.

## internal/smb2: what it is

- A vendored copy of github.com/macos-fuse-t/go-smb2 @`277a930` (docs/vendored.md, `internal/smb2/LICENSE`, `Attributions.txt`). It is not under `internal/thirdparty`, which holds only xorm and mpb. Local patches cover signing, compound validation, per-connection auth, cleanup, error statuses, xattr ranges, and new files cleanup.go, request_validation.go, status.go and xattr.go.
- Size is about 20.3k non-test Go lines, 3.6k of which are the generated ntstatus table. `server/` is 10.4k lines: file_tree.go 2627, server.go 1313, info_fs.go 1175, conn.go 573. The wire codecs in `internal/smb2/{request,response,fscc}.go` are about 4.2k.
- Code quality is prototype level. Examples:
  - CHANGE_NOTIFY is fake. It sends PENDING, sleeps 5 s, then completes with ACCESS_DENIED (file_tree.go:1659-1738).
  - Every standalone READ/WRITE sends an interim STATUS_PENDING and then an async final reply (file_tree.go:720-739). This doubles the responses and causes the credit bugs.
  - Leases are echoed back (`handleRqLs`) even though LEASING is not advertised.
  - DH2Q is granted unconditionally.
  - `MAXIMUM_ALLOWED` and `GENERIC_ALL` are not mapped.
  - `vfs/attributes.go` has explicit panics.
  - There is no `recover` anywhere.
- Patch or rewrite: patch the codecs and the conn/session/auth layer, which are now reasonably hardened and tested. Rewrite and simplify these parts:
  - (a) READ/WRITE: run concurrently but reply synchronously, with no interim PENDING. This removes #104 and most of #132.
  - (b) CHANGE_NOTIFY: return STATUS_NOT_SUPPORTED. macOS then falls back to polling. This removes #107 and #146.
  - (c) CREATE: rewrite as a small pipeline: parse name/stream → resolve target object → share-mode check → disposition → open → create contexts. A rewrite of file_tree.go into handler files around a shared "target object" helper (#123) is cheaper than patching 20 base/stream branches.
  - (d) Oplock, lease and durable code: strip it rather than complete it.
  - A full rewrite of the library is not justified.

## What TM over SMB needs

Apple's NAS guidance and Samba's vfs_fruit `time machine = yes` cover the following.

Required:
- SMB 3.x with signing.
- Bonjour `_adisk._tcp`.
- The AAPL create context with `kAAPL_SUPPORTS_FULL_SYNC`. This is already sent. It makes macOS turn F_FULLFSYNC into SMB2 FLUSH, so FLUSH must commit all of the file's data: #89 is core.
- Named streams, mainly `AFP_AfpInfo` (FinderInfo and the bundle bit on the `.sparsebundle`) and `com.apple.*` xattrs as `:name:$DATA` streams. AppleDouble `._` fallback exists, but it is untested here and vfs_fruit TM setups use streams. So keep streams and fix them.
- Compound requests. macOS uses CREATE+QUERY_INFO+CLOSE and CREATE+SET_INFO+CLOSE heavily.
- Multi-credit LARGE_MTU I/O. Bands are written in 1-8 MiB requests with CreditCharge > 1.
- Correct FsFullSize and FsAttribute replies.

Not needed for a backup to succeed:
- Leases and oplocks. They help performance only, and macOS works without them.
- Durable handles. The Mac acceptance runs pass without working ones. Apple lists durable handles v2 for surviving disconnects (#135), and doing them properly needs leases with handle caching, DH2C reconnect, and keeping opens and locks across sessions.
- Hard links, object IDs, open-by-file-ID, sparse FSCTLs, reparse points, DFS, CHANGE_NOTIFY, blocking locks and CANCEL. TM does not use these on SMB, because sparsebundle bands are plain files.
- O_EXLOCK/O_SHLOCK. macOS maps them to CREATE share modes (deny modes). DiskImages uses them to stop a second attach of the same bundle.

## Per-issue triage

| # | Title | Category | Sev | Rec |
|---|---|---|---|---|
| 84 | Truncate via 2nd handle undone by other handle's flush | race (shared writer) | critical | FIX |
| 85 | Exclusive oplocks granted to conflicting opens, no break | oplocks | medium | FIX-BY-REMOVAL (always grant NONE, don't echo RqLs) |
| 86 | ShareAccess ignored | share-modes | medium (DiskImages' double-attach guard) | FIX |
| 87 | GetAttr(0) → nil deref panics daemon (ObjectId/FS queries) | panic | critical | FIX (+ drop OBJECT_IDS flag) |
| 88 | FILE_SUPERSEDE keeps old contents | info-classes/create | low | FIX (add O_TRUNC, trivial) |
| 89 | FLUSH on 2nd handle doesn't commit other handle's writes | race (shared writer) | critical | FIX |
| 90 | Delete-on-close on a stream deletes the base file | delete-on-close/named-streams | critical | FIX |
| 91 | After recovery, retained disk cache serves old bytes (slice-ID reuse) | non-SMB: recovery | critical | FIX (recovery track) |
| 92 | Restored DelFiles delete data of an inode reused after recovery | non-SMB: recovery | critical | FIX (recovery track) |
| 93 | Delete-pending on a hardlink removes the wrong name | hardlinks | low (unreachable) | FIX-BY-REMOVAL via #122 |
| 94 | FILE_CREATE of a new stream → NAME_COLLISION | named-streams | medium | FIX |
| 95 | Durable/persistent handles granted, closed on disconnect | durable-handles | high | FIX-BY-REMOVAL (dup of #135) |
| 96 | SetAttr mtime overwritten by later flush | race (shared writer) | medium | FIX |
| 97 | FileStandardInformation on a stream returns the base size | named-streams/info-classes | medium | FIX |
| 98 | QUERY_DIRECTORY continuation without a name returns the dir itself | info-classes | medium | FIX (keep pattern on Open) |
| 99 | Case-sensitive lookups not advertised | capability-advertising | medium | FIX (advertise the truth) |
| 100 | Blocking LOCK in a compound ignores conn cancellation | async-cancel/locks | low | FIX (reject blocking locks in compounds, or go async) |
| 101 | Crashed session's locks survive restart (read-only forever) | locks (juicefs) | low | FIX (small) |
| 102 | SIGTERM with open handle: exit 1, pending delete lost | delete-on-close/shutdown | medium | FIX |
| 103 | Stream locks don't protect the stream and block base writes | locks/named-streams | low | FIX-BY-REMOVAL (LOCK on stream → NOT_SUPPORTED) |
| 104 | Async READ/WRITE lock-conflict reply has a sync header | async-cancel | low | FIX via simplification (no interim PENDING) |
| 105 | Wildcards treated as unescaped regex | info-classes | low | FIX (QuoteMeta, trivial) |
| 106 | SMB2 CANCEL dropped at top level | async-cancel | low | FIX (small) |
| 107 | Two CHANGE_NOTIFY on one handle race on AsyncId | race/async | low | FIX-BY-REMOVAL (NOT_SUPPORTED) |
| 108 | FileAllocationInformation below EOF doesn't shrink | info-classes | low | FIX (trivial) |
| 109 | FsFullSize ActualAvailable written at offset 18 | info-classes | medium (statfs, TM space checks) | FIX (1 char) |
| 110 | WithTimeout data race | race (juicefs) | low | FIX (trivial) |
| 111 | SET_INFO time -1 sets 2185, epoch read as "unset" | info-classes | low | FIX (trivial) |
| 112 | GENERIC_ALL (and MAXIMUM_ALLOWED) opens read-only | info-classes/create | medium | FIX (trivial) |
| 118 | Rename between hard links breaks handle paths | hardlinks | low | FIX-BY-REMOVAL via #122 |
| 122 | Advertises hardlinks, open-by-ID, sparse with no implementation | capability-advertising | medium | FIX-BY-REMOVAL |
| 123 | Base vs stream decided per handler | refactor | n/a | FIX (enabler) |
| 124 | Open/lock/delete state split across 3 layers | refactor | n/a | FIX partially (writer-per-inode, delete identity) |
| 129 | Concurrent senders share one werr channel | race | medium | FIX (branch swarm/64-sender) |
| 130 | Sender continues after partial write | race/framing | high | FIX (close conn on first write error) |
| 131 | zstd decode writes past destination subrange | non-SMB: corruption | critical | FIX (branch swarm/64-page-pool-v3, clamp cap) |
| 132 | Async READ/WRITE errors grant credits twice | credits | low | FIX via simplification |
| 133 | Sync compound WRITEs grant 0 credits | credits | low | FIX |
| 134 | NEGOTIATE can grant 0 credits | credits | low | FIX (min 1) |
| 135 | Durable promised, closed on disconnect; drop ends backup | durable-handles | high | FIX-BY-REMOVAL now; real DH2 later only after a Mac experiment proves it |
| 136 | Ignored errors (55 in smb2, 26 own) | umbrella | high | FIX |
| 137 | Panics kill the process; no recover | panic umbrella | critical | FIX |
| 138 | Capabilities advertised but not implemented | capability-advertising umbrella | high | FIX-BY-REMOVAL |
| 139 | smb2 library parent | umbrella | n/a | keep as tracker |
| 145 | Related compound doesn't inherit a non-CREATE FileId | compound | low | FIX (branch 895fad02) |
| 146 | Delayed CHANGE_NOTIFY resends an old compound response | compound/async | low | FIX-BY-REMOVAL (NOT_SUPPORTED) |

Notes on the severity calls:

- #84, #89, #96: DiskImages and smbfs can hold more than one fid per band, for example separate read and write fids. #89 breaks the F_FULLFSYNC promise.
- #90: deleting a band, `Info.plist` or the bundle directory is unrecoverable unless trash catches it. Whether macOS removexattr uses delete-on-close or a SET_INFO disposition is unverified, so fix it anyway.
- #85: CREATE handles only levels II and 8. A BATCH (9) request already falls to NONE, and macOS probably asks for that or a lease, so removing oplocks likely costs nothing. Check on a Mac.
- #99: the alternative fix is JuiceFS `CaseInsensi`, but only for new datasets.
- #87: crash by an ordinary query.
- #130: a misframed stream means a disconnect and a failed backup.
- #91, #92, #101 and #110 are JuiceFS or recovery issues, not SMB. Hand them to the storage/recovery plan.

## PR groupings (shared root cause)

- **PR-A, shared inode writer (#124 part 1):** #84, #89, #96. Flush the inode's single JuiceFS writer, not the handle's `wdata`, in `FS.Flush`, `Truncate` and `SetAttr`, or truncate the writer's buffered slices. One adapter-level helper, one PR. Highest priority.
- **PR-B, crash proofing:** #87 (separate root-attr/StatFS API, never handle 0), #137 (per-connection recover that closes only that connection, and turning `vfs/attributes.go` panics into errors), and the dropped-error subset of #136 that feeds nil results (GetAttr, StatFS, Getxattr, `res, _ := accept`).
- **PR-C, capability honesty (#138):**
  - Remove PERSISTENT_HANDLES and DFS from NEGOTIATE.
  - Stop answering DH2Q (#95, #135).
  - Always set oplock NONE and ignore RqLs (#85).
  - Remove HARD_LINKS, OPEN_BY_FILE_ID, SPARSE_FILES and OBJECT_IDS (#122, which makes #93 and #118 unreachable and shrinks #87's surface).
  - Fix the case flags (#99).
  - Add a test per advertised bit.
- **PR-D, sender (#64 track):** #129, #130, plus the send-error subset of #136.
- **PR-E, async simplification and credits:** synchronous replies for READ/WRITE (#104, #132), CHANGE_NOTIFY → NOT_SUPPORTED (#107, #146), credit floor and compound grants (#133, #134), CANCEL handling (#106), blocking lock in a compound (#100). #145 can go in here as well, being compound plumbing.
- **PR-F, named streams (after #123):** #90, #94, #97, #103. All come from "base or stream" being decided per handler. Build a `target` helper (size, xattr vs. data, delete) and a table test over disposition × base/stream, then fix the four bugs. #90 also needs delete identity (inode + stream) from #124.
- **PR-G, CREATE/SET_INFO trivia:** #88, #108, #111, #112, #105, #98, #109. Small, independent, one cleanup PR. #109 and #112 matter most.
- **PR-H, share modes:** #86, best done in the CREATE pipeline rewrite. It needs ShareAccess stored on Open and a check against existing opens on the inode.
- **Shutdown and recovery:** #102 (cleanup before protection closes), #101, and outside SMB #91, #92, #131, #110.

## Dependencies

- #123 comes before #90, #94, #97 and #103.
- The #124 writer-per-inode part is PR-A itself. The #124 delete-identity part comes before #90; #93 becomes moot after #122 removal.
- #136/#137 groundwork comes before broad smb2 refactors, so regressions surface as errors rather than nil derefs.
- PR-C (removing durable handles) comes before any real durable-handle design. A real DH2 needs leases (H caching) + break machinery (#85's dead `BreakNode`) + preserved opens/locks + DH2C. It depends on #64's root cause, because durable handles only hide the disconnect.
- PR-E's synchronous READ/WRITE removes #104 and most of #132. Do it before patching credits piecemeal.
- #86 is easiest after the CREATE restructure in PR-F.

## Subsumed by umbrellas

- **#135 / #95:** duplicates. Close #95 into #135. PR-C covers the removal step.
- **#137:** #87 (cause), the explicit panics in smb2, juicefs and thirdparty.
- **#136:** #87, the send-error parts of #129/#130, the async sends in #104 and #107, and `res, _ := accept` in about 20 handlers.
- **#138:** #85, #86, #88, #95, #99, #122 (→ #93, #118), #132, #133, #134, #135, DFS, leases.
- **#122:** #93, #118.
- **#139:** parent of everything in `internal/smb2`, that is all of the above except #84, #89 and #96 (adapter/juicefs), #91, #92, #101, #110, #131 (juicefs), and #102 (app).

## Priority for TM

1. #131, #84/#89/#96, #90, #87+#137.
2. PR-C (honest capabilities), #130/#129.
3. #109, #112, #98, PR-F stream fixes.
4. #86, #102.
5. All the rest.
