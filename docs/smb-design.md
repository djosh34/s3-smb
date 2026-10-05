# New SMB server design

The new SMB server serves one share to one Mac running Time Machine. It speaks
SMB 3.1.1 only, with NTLMv2 login, signing and AES-GCM encryption. It is built
with `-tags smbnext`; without the tag, s3-smb still uses the server in
`internal/smb-old`. There is no runtime setting to choose a server.

## Packages

- `internal/smb`: storage types, status mapping and the feature policy in
  `features.go`.
- `internal/smb/wire`: typed codecs for SMB messages.
- `internal/smb/auth`: NTLMv2 inside SPNEGO for the one configured user.
- `internal/smb/crypt`: preauth hashing, key derivation, signing and GCM.
- `internal/smb/state`: opens, sharing, deletion, byte ranges and leases, in
  memory.
- `internal/smb/server`: transport, sessions, trees, credits, compounds, async
  replies and the command handlers. It joins the packages above with
  `smb.Storage`.
- `internal/smbfs`: `smb.Storage` on JuiceFS.
- `internal/smb/smbtest`: a raw SMB test client and fixtures that serve a real
  JuiceFS adapter.

`internal/app` constructs the new server in files built with `smbnext` and the
old one in files built with `!smbnext`. No new package imports
`internal/smb-old`.

## Storage access

Storage access describes data permissions, not granted SMB masks.
`AccessRead` permits reads; `AccessWrite` permits writes and truncate.
`AccessAppend` restricts writes to offsets at or beyond the selected object's
live end of file, also when combined with `AccessWrite` for a destructive
CREATE. Alone, `AccessAppend` does not permit truncate. `Open` keeps these
permissions without creating or truncating data. `WriteAt` checks append access
inside the adapter's per-inode lock, together with every write and length
change. Each stream has its own end of file. The server still checks the
granted SMB access for each operation.

The wire codecs keep FILETIME sentinels until the handler interprets them.

`smbtest.NewStorage(testing.TB)` supplies a real JuiceFS adapter and registers
its cleanup; `smbtest.NewS3Storage` puts its objects behind an S3 fault proxy.
Server tests share one fixture in `server/fixture_test.go`: it serves such an
adapter on loopback through a hook-based fault storage and a fake clock, and
shuts the server down before the storage helper's cleanup runs.

## Handler request context

Handlers receive a cancellation context, a `server.RequestContext` and the wire
message. The request context holds immutable `Session` and `Tree` snapshots,
the shared `Opens` table and `Storage`. `Binding()` gives the identity used by
open-table methods. Dispatch validates the session and tree, not the file
handlers. Commands that need neither get zero snapshots. Handlers must not keep
snapshots as connection state. They do storage work outside table locks and
drain active handle users before cleanup. The connection core owns replies,
credits, async identity and message protection.

Session and tree IDs are unique across the running server. A tree belongs to
one session, even though every tree names the same share. LOGOFF and
TREE_DISCONNECT invalidate the identity, cancel and drain its work in flight,
then clean up its opens. Cleanup continues if the transport is canceled. On a
connection drop, durable opens are detached before waiting for work; storage
cleanup follows the drain.

## CREATE and cleanup

The server holds the parent namespace guard during CREATE. The adapter resolves
the name and selects the object once. The server reserves sharing before
changing an existing object. A missing file is created exclusively by the
adapter before the server reserves its identity. The server commits the
reservation only after the storage open succeeds; on failure it aborts the
reservation and closes every storage reference it took. The open table never
calls storage while holding its mutex.

A pending delete stays pending until CompleteDelete on every cleanup outcome,
including failure or cancellation. Cleanup resolves the inode's current path
under its parent guard and retries if a rename changed it. An inode without a
unique path is left alone. The adapter checks the expected inode before
deleting a name. The server drains active request references before closing a
storage handle, never while holding a namespace guard. A base-file rename locks
both parents in inode order. Open state follows the inode, not a cached path.
Renaming a named stream returns STATUS_NOT_SUPPORTED and changes nothing.
TREE_DISCONNECT closes every open of the tree, including durable opens.

## Features

`internal/smb/features.go` holds the exact masks the server advertises; each
contains only features whose handlers work. The share is case-sensitive and
supports named streams up to 64 KiB. The AAPL volume capabilities are
case-sensitive and full sync (0x06). The NEGOTIATE reply does not advertise
leasing. Hard links, open by file ID, sparse files, change notification,
classic oplocks, directory leases, durable v1 and persistent handles are not
granted.

SMB 3.1.1 negotiates encryption only through its encryption context. GCM is
required by default. The encryption setting can allow signed plaintext, but
never AES-CCM or an older dialect.

## Async I/O and credits

The server sends an interim STATUS_PENDING when READ, WRITE or FLUSH waits on
S3, after a short bounded wait for local work. A request that finishes locally
gets one synchronous reply. Each pending request owns its async ID and
completion state. The server keeps serving other requests and ECHO while S3 is
slow. The adapter retries transient S3 failures for five minutes; after that
the server returns the storage error.

The server validates the whole compound and checks request signatures and
credit charges before changing any state. A missing or bad signature, or
plaintext on an encrypted session, gets ACCESS_DENIED before any member is
dispatched. Invalid GCM authentication or malformed framing closes the
connection.

Each command consumes its credit charge once. For multi-credit commands the
charge is the larger of input and expected output, rounded up to 64 KiB units.
A synchronous reply and an interim reply each grant credits once; a final async
reply grants none, also on error, and keeps the request's MessageId, SessionId,
AsyncId and async flag. The sender writes complete frames in order and reports
each frame's result to its producer. A partial write closes the connection and
fails queued sends.

NEGOTIATE grants at least one credit and SESSION_SETUP at least five. The
server grows the balance toward 256 within the bound in `features.go`, and
replenishes credits before a valid synchronous compound can use up the
client's balance.

When a related compound member goes async, the rest of the compound runs
asynchronously too. Each dependent request gets its own pending reply and async
ID and waits for its predecessor, including CLOSE. The server carries the
session, tree and FileId from the preceding operation, also when that
operation used an existing handle. Handlers report the FileId they used or
created even on error; members that report none leave the saved ID unchanged.
An error from a predecessor fails a following related FileId command with the
same status; warnings do not. Completed replies are sent once. Unrelated
members need not wait on S3. CANCEL has no reply and cancels only the named
pending request. Byte-range lock requests never wait.

## Protection and reconnect

SMB 3.1.1 uses SHA-512 preauth integrity. The server prefers GMAC signing and
AES-256-GCM when offered; with no common signing algorithm it uses AES-CMAC, as
MS-SMB2 requires. Each session forks the NEGOTIATE transcript and hashes its
own SESSION_SETUP requests and challenge replies; keys use the hash through the
last request. Plaintext replies are signed, including the final SESSION_SETUP;
interim replies are unsigned, as the protocol allows. AES-GCM protects
encrypted traffic, including interim replies, and the server verifies the tag
before decoding. A reply to an encrypted request is encrypted even when
plaintext is allowed. A send nonce is never reused, and a reconnect derives
fresh keys.

Successful SESSION_SETUP with a PreviousSessionId of the same user removes the
old session with disconnect cleanup, which detaches durable opens instead of
closing them. Outstanding replies keep their key until the final response, so
replies after LOGOFF stay protected.

A connection drop detaches durable opens at once, without waiting for S3. The
server cancels the old requests but keeps acknowledged data and durable
handles. Detached opens keep their granted access, sharing, pending delete,
byte ranges and lease. Non-durable opens close after their active requests
drain. Old requests cannot publish grants or replies on the new connection.

Durable v2 handles are granted only for regular unnamed files that hold a lease
with H. A zero timeout gets 120 seconds; requests above 16 minutes get 16
minutes, reported in the reply. DH2C checks the file ID, CreateGuid, client
GUID, user, share and lease key, and the open gets a new volatile ID. A
CREATE whose CreateGuid is in use returns DUPLICATE_OBJECTID, also when marked
as a replay. Expiry and shutdown use the normal close path and apply pending
deletes.

A file has at most one lease. A conflicting open breaks it, waits for the
acknowledgment or a 35-second timeout, and tries once more; a lease still in
the way gives SHARING_VIOLATION. A timed-out break revokes the whole lease. A
lease whose opens are all detached drops at once, closing durable opens that
lose H.

## What survives a failure

- A connection drop of about 30 seconds can continue the same backup. An
  unanswered CREATE, LOCK or SET_INFO can make macOS refuse to reconnect.
- A longer drop fails the current backup visibly; earlier backups stay intact.
- A connection drop keeps acknowledged work.
- A crash with the local disk intact keeps flushed work.
- Recovery on a new Mac returns the last hourly metadata backup.
- FULL_SYNC waits for the S3 data, the local metadata commit and a full-fsync
  barrier, not for a metadata backup.

## Tests

Every advertised capability needs a raw Go test or a Samba test in
`test/e2e/smbtorture.allowlist`. The allowlist names exact smbtorture test IDs
from the Samba version pinned in `test/Dockerfile`; a listed test that fails or
is skipped fails the check. Tests that need features the server does not
support are not listed. See [development](development.md#docker-integration).
