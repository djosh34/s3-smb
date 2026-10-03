# Research: from-scratch SMB server rewrite (s3-smb, main @ac712a6)

Builds on research-smb.md (issue triage) in this folder. How this was measured: `go test -cover` on a throwaway worktree. smbtorture 4.17, smbclient and a privileged `mount -t cifs` ran in Docker against the current server: internal/smb2 + smbfs on a file-backed JuiceFS, through a 60-line harness copied from the smbfs test fixture. The worktree, image and harness have been removed.

Key findings:
- Little in internal/smb2 is worth porting. The exceptions are NTLM/SPNEGO, AES-CMAC and the project-added helpers, all after review. The valuable part is the docs/vendored.md patch list, which reads as a list of requirements.
- A new server is about 9-11k non-test lines (today 20.3k), in 6 milestones. M1 is five independent packages that agents can build in parallel.
- The boundary: the SMB layer keeps every open, share mode, delete-pending flag, lock and lease in memory, in an open table that does not depend on connections. The filesystem layer handles paths, coherence per inode and streams stored as xattrs, and knows nothing about opens. JuiceFS plocks are dropped.
- No existing Go library is good enough to adopt.
- smbtorture, smbclient and the cifs mount all work in Docker. smbtorture crashes today's server (#87). It also shows that DH2C is ignored and handled as a plain open. Run it from an allowlist of test names, not whole suites.

## 1. Inventory of internal/smb2

20,335 non-test lines, 3,440 test lines. The wire layer, NTLM and SPNEGO come from hirochachacha/go-smb2, which is a client. So about half of the codec code is the client direction (request encoders, response decoders) and is never used by the server.

| Part | Lines | Quality / coverage | Patches | Verdict |
|---|---|---|---|---|
| `internal/smb2/{request,response}.go`: command codecs, lazy `[]byte` decoder views | 3,480 | Mechanical, `IsInvalid()` bounds checks, half unused; package 8% covered | CREATE/WRITE bounds, WRITE data offset | Reference only. Rewrite the server direction and fuzz it |
| `fscc.go` + `server/info_fs.go`: info classes | 1,905 | Duplicated; #109 offset bug | – | Reference; rewrite the ~15 classes TM uses |
| `packet, smb2, const, util, dtyp, oplock`: header, constants, FILETIME, GUID, break codecs | 1,650 | Fine | – | Port constants after review |
| `dcerpc.go` + `server/ipc_tree.go`: IPC$ srvsvc share enumeration | 830 | Prototype, untested | – | Drop. TM gets the share name from Bonjour `_adisk` |
| `internal/erref`: generated NTSTATUS table | 3,598 | 0% | go:generate removed | Drop; keep about 60 constants |
| `internal/utf16le` | 56 | 0% | – | Drop (`unicode/utf16`) |
| `internal/ntlm` (server half used, client.go dead) | 1,220 | Close to MS-NLMP. Slices like `ntChallengeResponse[16:]` have no explicit length checks, and responses are compared with `bytes.Equal`. 68% covered | – | Port the server half (~400 lines) after review, plus MS-NLMP test vectors |
| `internal/spnego` (`encoding/asn1`) | 282 | OK, 29% covered | – | Port after review |
| `internal/crypto/{cmac,ccm}` | 435 | Standard ports, 81-82% covered | – | Port cmac. Drop ccm (stdlib GCM covers encryption) |
| `server/kdf.go`, `session.go` signing and encryption | ~160 | One `hash.Hash` per session is shared by every sender. Responses are signed with `conn.session`, not the request's session | verify each compound part | Rewrite (small) |
| `server/{conn,transport,compound,credit}.go`: framing, receive loop, sender, compound, credits | 810 | #129, #130, unsynchronised `sequenceWindow`, #132-134, one session per connection | compound validated first, close on bad signature, SMB1 only as first packet | Rewrite; keep the patch rules as requirements |
| `server/server.go`: negotiate, session setup, tree connect, open table, delete-pending, break stubs | 1,313 | `Open` has ~45 fields, mostly unused; delete-pending is keyed by inode only | per-connection auth, session gate, logoff cleanup, shutdown waits | Rewrite; negotiate contexts as reference |
| `server/file_tree.go`: every file handler | 2,627 | Base or stream is decided per handler (#123). CHANGE_NOTIFY is fake. Every READ/WRITE sends an interim PENDING. DH2Q is granted and RqLs echoed. ~38% covered | error statuses, write-through, stream ranges, handle/session/tree checks | Rewrite; use it as a checklist of what macOS sends |
| `server/locking.go` | 206 | SMB lock table mirrored into JuiceFS plocks (#124) | – | Rewrite as an in-memory table |
| `server/acl.go` | 277 | Prototype | – | Reference only |
| Project-added `xattr.go`, `cleanup.go`, `status.go`, `request_validation.go` | 350 | Ours, tested | – | Port the logic |
| `vfs/`: `VFSFileSystem`, `Attributes` getters that `panic` | 650 | Ambiguous between root and handle 0 (#87) | `vfs/xattr.go` | Drop; new interface (section 2) |

Two further defects showed up in testing. `smb2.compound` kills the process with a nil dereference: `handleCreateOrGetObjectId` reaches `vfs/attributes.go:144` (#87). Also, any unsupported info class closes the connection. smbclient `allinfo` asks for FileAlternateNameInformation (21), and `file_tree.go:2068` returns `InvalidRequestError` instead of a status. This second defect does not appear to be filed.

## 2. Boundary

How the state is split today. `smbfs.FS` implements `vfs.VFSFileSystem` plus `ByteRangeLocker`: `Open/OpenDir/Mkdir` take a path, and everything else takes an opaque `VfsHandle`. State lives in three places:
- **SMB server:** `Server.opens`, `opensByGuid`, `deletePending map[inode]bool` and the SMB lock table. A named stream is an `Open` with `isEa/eaKey` on top of the base file's handle.
- **smbfs:** `handles` holds the JuiceFS `*File`, path, flags, directory cursor and lock ranges. Renames rewrite the paths of descendant handles, and locks are mirrored into plocks keyed by handle number.
- **JuiceFS:** one writer per inode, shared by all handles. Plocks are stored in SQLite and survive a restart (#101).

Proposed split:

**The SMB layer owns everything with SMB meaning, in memory:**
- connections, sessions, trees, credits, signing, compound requests and async IDs;
- the open table, keyed by persistent FileId and independent of connections: `Open{id, createGuid, user, share, objectKey, access, shareAccess, deleteOnClose, lease, durableDeadline, dirCursor, fsHandle, session}`. `session` is nil while a durable open waits for its client to reconnect;
- a record per object, keyed by `(inode, stream)`. It holds the opens (for the share-mode check), the delete-pending flag, byte-range locks (non-blocking, unlocked by exact range) and leases by lease key;
- CREATE as one pipeline: parse name and stream → resolve the object → access and share-mode check → disposition → FS open or create → create contexts;
- shutdown in order: stop accepting, drain requests, close all opens (which applies delete-on-close), then return to the app, which closes JuiceFS (#102).

**The filesystem layer (new smbfs) owns everything about storage and knows nothing about opens:**
- path resolution, plus the rules that forbid the trash, special nodes and `..` traversal;
- objects named by `(Ino, stream)`. A stream is an xattr (64 KiB limit) behind the same `ReadAt/WriteAt/Size/Truncate/Remove` calls as a file, so the SMB layer never asks whether something is a stream (#123);
- a handle that is only a JuiceFS file and an access mode;
- coherence per inode: `Flush`, `Truncate`, `SetAttr` and `GetAttr` work on the shared writer (#84, #89, #96, #113);
- `Remove(parent, name, expectIno)` and `Rename` with identity checks, plus `PathOf(ino)`, which is unique without hard links;
- `StatFS()` and `RootAttr()` as separate calls, so there is no handle 0 (#87);
- no locks. Plocks are not needed because one daemon owns the dataset (the app lock file), so #101 and the double bookkeeping go away.

Interface sketch (~17 methods): Lookup, Open(ino, stream, access), Create, Close, ReadAt, WriteAt, Flush, Truncate, GetAttr, SetAttr, ReadDir(ino, cookie), Streams, Remove, Rename, PathOf, StatFS, RootAttr. Errors are `syscall.Errno`, and the SMB layer owns the map to NTSTATUS.

Durable reconnect needs nothing from the filesystem layer:
- On disconnect, a durable open keeps its FS handle, so buffered writes, delete-pending and locks survive. It drops its session and gets a deadline.
- DH2C finds the open by FileId, CreateGuid, the same user and the lease key, and attaches it to the new session and tree.
- A scavenger closes expired opens through the normal close path.

## 3. Existing Go servers

None is worth adopting:
- **ahmetozer/gosamba** (Apache-2.0, 58 commits, 1 star): 3.1.1, signing, encryption, durable handles, AAPL, streams. Leases are always granted as NONE, so it cannot do spec-correct durable v2. Internal packages only.
- **sonroyaalmerol/go-smb-server** (MIT, 46 commits): pluggable VFS. No durable handles, leases, streams or AAPL are mentioned. Tested only with basic smbclient operations.
- **go-filesystems/smb** (BSD-3, 21 commits): no 3.1.1, durable handles, leases or streams. It has been tested against macOS, Linux and Windows clients.
- **jfjallid/go-smb `smb/server`** (MIT, v0.12, pre-1.0): durable handles. Built as security tooling; no leases or streams.

gosamba's durable store is worth reading as a reference.

## 4. Size, layout, milestones

Estimated non-test lines:

| Component | Lines |
|---|---|
| wire (header, 16 commands, create contexts AAPL/MxAc/QFid/DH2Q/DH2C/RqLs, ~24 info classes, security descriptor) | 2,800 |
| auth (ported NTLMv2 + SPNEGO) | 650 |
| crypt (KDF, preauth, CMAC/GMAC signing, optional GCM encryption) | 350 |
| server core (framing, single ordered sender, sessions, credits, compound, dispatch) | 1,400 |
| negotiate, session setup, tree, echo, logoff | 600 |
| state (opens, objects, share modes, delete-pending, locks) | 700 |
| CREATE pipeline | 900 |
| other handlers (close, flush, read, write, query/set info, query directory, lock, ioctl subset) | 1,500 |
| leases, breaks, durable v2, reconnect, scavenger | 900 |
| adapter | 1,200 |

The total is about 11k. Tests are 8-12k more.

Layout: `internal/smb/wire` (pure, fuzzed), `internal/smb/auth`, `internal/smb/crypt`, `internal/smb/state` (pure, clock injected), `internal/smb/server` (core plus create.go, io.go, info.go, dir.go, lock.go, ioctl.go, lease.go, and the FS interface), `internal/smb/smbtest` (real adapter on a file-backed JuiceFS, server on 127.0.0.1:0), and `internal/smbfs` (the adapter).

Milestones:
- **M0** (1 agent, owner review): the FS interface, state invariants, status map, the exact list of advertised capabilities, and the smbtorture allowlist policy.
- **M1** (5 agents in parallel): (a) wire plus fuzz targets; (b) auth with MS-NLMP test vectors; (c) crypt with MS-SMB2 test vectors; (d) state, with table tests for the share-mode matrix, delete-pending, lock conflicts and durable expiry on a fake clock; (e) the adapter, with fixture tests for coherence, streams and identity-checked remove and rename.
- **M2** (1-2 agents): negotiate (3.0.2 and 3.1.1), session setup, signing, tree, echo, logoff, credits, compound plumbing. Passes when smbclient `ls`, `smb2.credits` and the hirochachacha client work.
- **M3** (3-4 agents, one per handler group): create/close/read/write/flush, query/set info, query directory, rename/delete. Passes `smb2.read`, `rw`, `getinfo`, `setinfo`, `dir`, `rename`, the compound related/unrelated/invalid tests, and test/e2e.
- **M4** (parallel): streams, AAPL and FsFullSize; share modes and delete-on-close; non-blocking locks; the ioctl subset. Passes the `smb2.streams`, `sharemode`, `delete-on-close-perms` and non-blocking `lock` subsets, then a first Mac run.
- **M5** (1-2 agents): leases (RH/RWH, break on a conflicting open, CREATE goes async while it waits for the break acknowledgement), durable v2, DH2C and the scavenger. Passes the `smb2.lease` and `smb2.durable-v2-open` reopen subsets, a cifs drop test on Linux, and the #150 Mac drop test.
- **M6**: flip and delete (section 6).

## 5. Testing

Tests to keep:
- **test/e2e** (20 tests): the binary, MinIO and the hirochachacha client. Fully black-box, so it runs unchanged against both servers. That client has no leases or durable handles, so it cannot test M5.
- **test/macos**: black-box, the final gate.
- **smbfs tests** (78% coverage, real JuiceFS): the scenarios port to the new interface; the lock tests move to the state package.
- **internal/smb2/server tests** (37.6% coverage, 27 files): almost all are white-box. Do not port the code. Rewrite the scenarios of the wire tests (auth, compound, error, negotiate, malformed, session_gate, xattr range/resize/concurrency, durability, delete_disposition, lock) as black-box tests against smbtest. Each one encodes a patch requirement.

**smbtorture works.** On debian:bookworm-slim, `apt-get install samba-testsuite smbclient cifs-utils`, then run `smbtorture //127.0.0.1/tm -p PORT -U u%p smb2.X` without privileges. Results against today's server (pass/fail/skip):

| Suite | Result | Notes |
|---|---|---|
| connect | 0/1 | info-class disconnect |
| read | 0/4/1 | |
| rw | 2/1 | |
| compound | 1/18 | server crashed (#87) |
| credits | 3/0 | |
| streams | 0/13/1 | |
| lock | 4/18/3 | blocking-lock tests hang |
| create | 6/9/+1 error | |
| getinfo | 0/7/1 | |
| setinfo | 0/1 | |
| dir | 2/5/1 | |
| rename | 5/6 | |
| delete-on-close-perms | 0/9 | |
| sharemode | 1/2 | #86 |
| ioctl | 1/43/29 | |
| session | 0/21/45 | most need batch oplocks |
| durable-v2-open | 0/10/5 | DH2C is ignored: a reconnect while the file is still open returns OK |
| durable-open | 0/7/3 | smbtorture 4.17 segfaulted |
| lease | 0/0/39 | LEASING is not advertised |
| timestamps | 1/14 | |
| oplock | 1/41 | |
| fileid | 2/2 | |
| compound_find | 1/2 | |

notify and maxfid were not run.

Many tests assume ACLs, impersonation, batch oplocks, copychunk or sparse files, so run smbtorture from a checked-in allowlist per milestone and let CI fail on any listed test. Examples:
- durable-v2-open: reopen1, reopen1a, reopen2, reopen2-lease, reopen2-lease-v2, open-lease
- lease: request, break, nobreakself, upgrade, v2_request, timeout-disconnect
- streams: io, names, create-disposition, delete, zero-byte
- compound: related1-9, unrelated1, invalid1-4, create-write-close
- credits.*, sharemode.*
- lock: valid-request, rw-*, auto-unlock, errorcode, zerobytelength

Run the full suites weekly for information only. Pin a newer smbtorture (trixie 4.22, or a source build as test/Dockerfile does for MinIO).

**smbclient** works unprivileged in the same image. It found the info-class disconnect on its first command.

**cifs** mounts in `docker run --privileged --network host` on this host (3.1.1 with AES-128-GCM negotiated; an 8 MiB write worked). test/Dockerfile and scripts/test-linux.sh do not use privileged mode today. The ubuntu-24.04-arm runner has sudo and Docker, but whether its kernel has `cifs.ko` needs a probe step first. The value is that the Linux client requests leases and durable v2 handles and reconnects by itself. A TCP proxy that cuts the connection under a cifs mount would test M5 in Linux CI, which overlaps with #142. Put it in a separate job.

**Fuzzing** (native Go: short runs in CI, long runs nightly):
- one target per decoder (header, each request, the create-context chain, compound split, SET_INFO classes, security descriptor, NTLM messages, SPNEGO), checking for no panic and a clean round trip;
- one stateful target that feeds framed streams to a server over `net.Pipe` with the real adapter, and checks for no panic and no hang, and that the server always either replies or closes;
- seed corpora from smbclient, smbtorture and Mac captures.

## 6. Switch-over and issues

Steps:
1. Build `internal/smb` and the new adapter next to `internal/smb2`.
2. `serve.go` builds the server in one function, with two build-tagged files (default and `smbnext`). There is no public config switch.
3. CI runs test/e2e and the smbtorture allowlist against both binaries. The Mac workflow gets a tag input.
4. Flip the default in one PR once the new binary passes everything the old one passes, plus its allowlist and two Mac runs.
5. In the next PR, delete `internal/smb2`, the old smbfs, the tag, the SMB section of vendored.md, the go-smb2 NOTICE/Attributions entries and its packaging_test entry. Code ported from ntlm, spnego and cmac keeps its attribution.
6. M5 can land after the flip. Until then, advertise no leases and grant no durable handles (#150 option 2).

**Closed by the rewrite:** #85, #86, #87 (server side), #88, #90 (with the adapter's `(ino, stream)` identity), #93, #94, #95, #97, #98, #99, #100, #103, #104, #105, #106, #107, #108, #109, #111, #112, #118, #122, #123, #129, #130, #132, #133, #134, #138, #139, #145, #146, and the SMB parts of #124, #125, #136 and #137. #135 closes when M5 lands. The unfiled info-class disconnect also goes away.

**Fixed outside smb2:**
- Adapter: #84, #89, #96, #113, #59, #101 (drop plocks), the rest of #124.
- JuiceFS: #110, #128, #131, #144.
- App, recovery and config: #91, #92, #114, #102 (shutdown order), #58, #60, #61.
- Tests and tooling: #115, #116, #117, #119, #120 (smbtest covers two handles and two connections over the real adapter), #121, #126, #127 (docs must list the new SMB subset), #141, #142, #143.
- #64 stays open. The new framing code should log the header and length of any rejected frame. Durable reconnect only hides the problem.
- #67-#83, #140 and #147-#157 are reviews, umbrellas and decisions.
