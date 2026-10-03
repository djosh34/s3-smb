# New SMB server design

This is the design for [M0](https://github.com/djosh34/s3-smb/issues/186).
The final comments on [the feature decision](https://github.com/djosh34/s3-smb/issues/169)
and [the playbook](https://github.com/djosh34/s3-smb/issues/176) take precedence over the research.

## Packages and interfaces

`internal/smb` holds storage types, NTSTATUS mapping and feature constants.
`wire`, `auth`, `crypt` and `state` import it, but not each other.
`server` joins those packages. `internal/smbfs` implements `smb.Storage` on JuiceFS.
`smbtest` uses wire, auth and crypt for a raw client, and server for real-adapter fixtures.
No new package imports `internal/smb-old`.

The Go interfaces and comments are the M1 contract. Constructors documented in
package comments are provided by their owning milestone, not stubbed in M0.
`wire.Codec` accepts only its listed concrete body types. Its raw message interface
lets tests send precise headers, compounds and malformed bytes without adding
unsafe modes to production decoders. M1 owns all body and context codecs; M2 owns
session policy, transport and dispatch. Adapter errors cross the seam as wrapped
`smb.ErrorKind`; state returns command-specific NTSTATUS values directly.

CREATE holds a parent namespace guard through lookup, share reservation,
disposition, storage open and commit. Missing files are created exclusively before
reserving their new identity. Existing files are reserved before any truncate.
Abort and close every acquired reference on failure. Table methods return before
storage I/O. Close takes the same namespace guard before removing table state and
performing identity-checked deletion. Rename guards both parents in inode order;
open state follows the inode, not cached paths. The adapter selects streams once,
so every handler uses the same storage calls for files and streams.

## Build selection

Until M6 the default build uses `internal/smb-old`. M2 wires the new server into
`internal/app` using two constructor files with `!smbnext` and `smbnext` build tags.
There is no runtime server-selection setting. M0 creates no main wiring.
The encryption setting is separate: GCM is required by default; turning it off
permits signed plaintext, not another dialect or AES-CCM. M6 flips the default,
runs the full suite and two green Mac runs, then deletes the old server and tag.

## Async I/O and credits

READ, WRITE and FLUSH that wait on S3 send an interim STATUS_PENDING before the
client's synchronous timeout, including cold-cache reads and full-sync uploads.
Use a short bounded fast-path wait; local completed I/O sends only one reply.
A request owns its async ID and completion state, never an open or old compound.
Other requests and ECHO continue while S3 is slow. Retry transient storage failures
for the five-minute outage window; beyond it return a visible storage error.

Validate the whole compound, signatures and credit charges before dispatch.
Charge each member once, using ceil(max(input, expected output)/64 KiB) where the
command requires it. A synchronous reply grants credits once. An async interim
reply grants credits once; its final success or error grants zero, retaining the
original MessageId, SessionId, AsyncId and async flag. Never grant twice on errors.
The sender writes each complete frame in order and reports that frame's result to
its producer. A partial write failure closes the connection and fails queued sends.

For a related compound, once a member goes async, process every dependent suffix
member asynchronously too, including CLOSE. Give each its own pending response
and async ID; do not execute it before its prerequisite finishes. Save inherited
session, tree and FileId from the preceding operation, including an existing-handle
operation, not only CREATE. Emit completed prefix replies only once. Final replies
may be compounded or separate, never appended to an already-sent buffer. Unrelated
members need not wait on S3. See MS-SMB2 sections 3.3.5.2.7.2 and 3.3.4.2.
CANCEL has no response and cancels only its identified pending request; locks never queue.

NEGOTIATE grants at least one credit even for a zero request. Login grants at
least five for reconnect; grow toward 256 without exceeding the configured bound
in `features.go`. Reserve sufficient response capacity so valid synchronous
compounds cannot consume the last credits without replenishment.

## Protection and reconnect

3.1.1 uses SHA-512 preauth, NTLMv2/SPNEGO and session-derived keys. Select only an
offered algorithm: prefer GMAC over CMAC and AES-256-GCM over AES-128-GCM.
Plaintext authenticated traffic is signed, including the final SESSION_SETUP;
interim pending replies follow MS-SMB2's unsigned-interim exception. For encrypted
sessions, AES-GCM encrypts and authenticates the entire payload, including interims.
Do not sign separately. Verify the tag before decoding plaintext. Never reuse a
nonce. Each session owns its protector; reconnect derives fresh keys and nonce state.

On a transport drop, durable opens detach but retain handles, buffered data,
sharing, deletion intent, locks and leases. Cancel old request contexts and detach
immediately so reconnect does not wait on S3. Defer storage close until active
request references drain. Non-durable opens close normally. Late old requests
cannot publish grants or replies on the new binding.
DH2C checks both FileId halves, CreateGuid, client GUID, user, share and lease key,
then rebinds to the new session/tree with a fresh volatile ID. Accept
PreviousSessionId, reconnect negotiation with the old algorithm alone, and AAPL
again on the new connection. Attached, expired or mismatched opens cannot reconnect.
Expiry and shutdown use the normal close/delete path. Replay CREATE must match its
original identity and parameters; an unmarked duplicate gets DUPLICATE_OBJECTID.
No persistent state survives a daemon restart.

A normal traffic drop of about 30 seconds can continue the same Mac backup.
An unanswered CREATE, LOCK or SET_INFO may make macOS refuse reconnect. Longer
drops fail that backup visibly; earlier backups remain intact and the next works
([reconnect limits](https://github.com/djosh34/s3-smb/issues/266)). Connection drops
keep acknowledged work, crashes with local disk intact keep flushed work, and
machine-loss recovery returns the last hourly metadata backup
([failure rules](https://github.com/djosh34/s3-smb/issues/263)). FULL_SYNC waits for
S3 data, metadata commit and a local full-fsync barrier, not an hourly backup.

## Milestones and tests

These lists replace later-feature dependencies in the starting plan
([milestone rules](https://github.com/djosh34/s3-smb/issues/267)). Each issue needs
its own regression. PR checks replay fuzz seeds; gates fuzz every target for one
minute. Race/shuffle tests cover all new packages. Mac gates use only features
present at that milestone; the first backup acceptance is M4.

- **M1, core packages (#187):** wire (#200) tests header, command, context, info,
  security and compound codecs, offsets, FILETIME sentinels and round trips, with
  one fuzz target per decoder. Auth (#201) uses MS-NLMP vectors, MIC and bad-proof
  tests, malformed SPNEGO/NTLM fuzz seeds and the test initiator. Crypt (#202) uses
  MS-SMB2 vectors, both GCM key lengths, wrong tags, direction, concurrent signing
  and unique nonces. State (#203) tests both directions of the share matrix,
  reservations/rollback, delete-pending, stream isolation, exact unlock, atomic
  vectors, range overflow/zero-byte rules and durable expiry on a fake clock.
  Adapter (#268) tests real file-backed JuiceFS with two handles: flush on the
  non-writer, upload/barrier failure, truncate then flush, timestamp then flush,
  live lookup/list size, unrelated progress during a slow flush, stream ranges,
  size limit, all dispositions' storage primitives, identity-checked remove/rename,
  read-only mode, separate root/space calls and configured/default capacity.
  Raw client (#269) tests framing, exact credit/header preservation, interim/final
  correlation and malformed send. S3 proxy (#271) tests delayed headers/bodies,
  errors, throttling, cut bodies and timed outages without a running SMB server.
- **M2, connection setup (#188):** raw client and smbclient authenticate and
  tree-connect, without listing or file access. Test opening SMB1 wildcard reply,
  3.1.1-only refusal, missing GCM refusal, one user, no guest/Kerberos/IPC$,
  preauth, signed final setup, every plaintext compound signature, GCM tampering,
  multiple sessions, tree/session gates, echo/logoff, initial and multi-credits,
  ordered sender completion and fail-stop after partial writes (#129, #130).
  Framing tests use ECHO compounds, not file operations. Check async plumbing with
  controlled completion, including final error identity/zero credits (#104, #132).
  The stateful real-adapter net.Pipe fuzz target (#270) always replies or closes,
  with bounded time. Allowlist only credit cases requiring no file handlers.
- **M3, file operations (#189):** raw two-open/two-connection tests cover CREATE
  dispositions including supersede, GENERIC_ALL, close, read/write-through,
  cross-handle flush/full-sync barriers, truncation and time sentinels. Test live
  directory sizes, empty-pattern continuation, literal/DOS wildcards, rename,
  delete and FsSize/FsFullSize field offsets. Unsupported info classes and object-ID
  requests return a status, followed by a successful ECHO (#87, #204).
  Delayed cold GET/PUT tests (#209) verify interim replies, final success/error
  identity, no duplicate credit grants and unrelated progress through a five-minute
  S3 outage. Run smbclient directory listing and the read/rw/getinfo/setinfo/dir/
  rename/related-unrelated-invalid compound allowlist. Check in an e2e subset:
  TestSMBToS3Smoke, TestFilesystemOperations, TestAuthentication, TestReadOnly,
  TestRecovery, TestMissingDataIsSMBError and TestZeroCacheUnusableDirectory.
  No stream, AAPL or lease requirement yet. Sharing reservations are enforced by
  M3 CREATE because destructive dispositions cannot precede their share check;
  M4 adds the full interoperability matrix.
- **M4, Mac features (#190):** raw and e2e stream tests cover all dispositions,
  offset/resize/limit, selected-object info and close sizes, zero-byte AAPL stream
  opens returning OBJECT_NAME_NOT_FOUND, stream delete-on-close with base/other
  stream opens, disconnect deletion and stream lock isolation. Check share modes,
  deny-delete rename, delete-on-close permissions, atomic non-blocking locks,
  immediate refusal of waiting locks, CANCEL's no-reply rule and CHANGE_NOTIFY
  NOT_SUPPORTED without a delayed completion. Check every advertised filesystem/
  AAPL bit, case-sensitive lookup/rename, no hard-link/open-ID/sparse grants and
  no plocks after restart. Run streams/sharemode/create/delete-on-close and
  non-blocking lock allowlists plus TestResourceForkOffsetsAndResize. The Mac gate
  (#205) checks xattrs, FinderInfo, sparsebundle create/attach, F_FULLFSYNC and
  a backup. Advertise no leasing or durability until M5.
- **M5, reconnect (#191):** raw clients through a pure Go fault proxy (#206) test
  R/RH/RWH V2 grants, conflicts/break/ack/timeouts, no classic oplock or directory
  lease grants, H-only regular-file durability, exact requested timeouts through
  16 minutes and zero -> 120 seconds, duplicate/replay CREATE, disconnect retention
  and identity-checked DH2C. Fake-clock expiry tests verify storage cleanup and
  pending deletion. Drops during read/write/flush preserve acknowledged data,
  ranges and sharing across two connections; new keys and volatile IDs reject
  stale traffic. Run lease and durable-v2 reopen allowlists. The Mac drop gate
  (#207) cuts during band writes and checks the same backup completes. A macOS
  refusal counts as not tested, retried at most three times, then recorded while
  Go and smbtorture decide M5. A separate >30-second drop checks visible failure,
  intact earlier backups and a successful next backup. No kernel CIFS mounts,
  privileged Docker or packet-filter commands.

## smbtorture policy

Each milestone checks in exact test names from a pinned Samba version, not suite
wildcards. Enumerate them before enabling them; names in the research are only
candidates. A listed failure fails the gate. Unsupported ACL, impersonation,
blocking-lock, batch-oplock, copychunk and persistent-handle cases are not gates.
Full suites may run for information, but never replace the allowlist. Every
advertised capability needs a raw Go or listed Samba test. M6 runs the full e2e
suite and accumulated allowlists against the new default before deleting the old
server. No GitHub milestone issues are edited by this PR; the manager copies these
lists and records any later changes with their reason.
