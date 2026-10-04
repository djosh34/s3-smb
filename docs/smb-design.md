# New SMB server design

This is the contract for [M0](https://github.com/djosh34/s3-smb/issues/186).
Final decision comments and [the playbook](https://github.com/djosh34/s3-smb/issues/176)
take precedence over the research.

## Packages and contracts

`internal/smb` holds storage types, status mapping and feature policy.
`wire` owns plain typed codec functions.
`auth`, `crypt` and `state` own authentication, message protection and open state.
`server` joins them with `smb.Storage`.
`internal/smbfs` implements storage on JuiceFS.
`smbtest` owns the raw client and real-adapter fixtures.
No new package imports `internal/smb-old`.

Code comments pin future function and method signatures.
M1 adds their bodies and private state, without M0 stubs.
Modules with one implementation return concrete pointers.
Storage and the injectable metadata barrier remain interfaces.
The class types and codec contracts are in `wire/info.go` and `wire/info_api.go`.
The wire codecs preserve FILETIME sentinels until the handler interprets them.

The raw client issue (#269) starts after the wire PR (#200) merges, still in M1.
Its M1 scope is framing and exact messages.
M2 (#188) adds client login, signing and encryption against a running server.

## CREATE and cleanup

The server holds the parent namespace guard during CREATE.
The adapter resolves the name and selects the object once.
The server reserves sharing before mutating an existing object.
For a missing file, the adapter creates it exclusively before the server reserves its new identity.
The server commits the reservation only after storage open succeeds.
On failure, the server aborts its reservation and closes every storage reference it acquired.
The table releases its mutex before the server calls storage.

The server takes the namespace guard before removing an open from the table.
The adapter verifies the expected inode before deleting a name.
The server drains active request references before closing their storage handle.
For a base-file rename, the server locks both parents in inode order.
Open state follows the inode, not a cached path.
The adapter refuses named-stream rename with STATUS_NOT_SUPPORTED and leaves data unchanged.
TREE_DISCONNECT closes every tree open, including durable opens.

## Build and feature selection

The old server remains the default until M6.
M2 wires the new server into `internal/app` with `!smbnext` and `smbnext` constructor files.
There is no runtime server-selection setting.
M0 does not wire main.
M6 switches the default after the full suite and two green Mac runs, then removes the old server and tag.

Each advertised mask contains only features whose handlers work at that milestone.
M4 adds the named-stream bit and enables the AAPL volume mask of 0x06.
Case-sensitive is advertised to fix #99.
M5 adds the leasing bit to the NEGOTIATE mask.
SMB 3.1.1 negotiates encryption only through its encryption context, not a global capability bit.
GCM is required by default.
The encryption setting can permit signed plaintext, but never AES-CCM or another dialect.

## Async I/O and credits

The server sends an interim STATUS_PENDING when READ, WRITE or FLUSH waits on S3.
The server allows a short bounded wait for local work before choosing that reply.
A request that finishes locally gets one synchronous reply.
Each pending request owns its async ID and completion state.
The server continues serving other requests and ECHO while S3 is slow.
The adapter retries transient S3 failures for the five-minute outage window.
After that window, the server returns the storage error visibly.

The server validates the whole compound before dispatch.
The server verifies request signatures and credit charges before changing state.
Each command consumes its credit charge once.
For multi-credit commands, the charge rounds the larger of input and expected output up to 64 KiB units.
A synchronous response grants credits once.
An interim response grants credits once.
A final async response grants zero credits, including on error.
The final response retains the request's MessageId, SessionId, AsyncId and async flag.
The sender writes complete frames in order.
The sender reports each frame's own result to its producer.
A partial write error closes the connection and fails queued sends.

When a related compound member goes async, the server also processes its dependent suffix asynchronously.
Each dependent request gets its own pending reply and async ID.
The server waits for the prerequisite before executing a dependent request, including CLOSE.
The server saves inherited session, tree and FileId from the preceding operation.
This inheritance also applies when the preceding operation uses an existing handle.
The server sends completed prefix replies only once.
Final responses use fresh buffers and may be compounded or sent separately.
Unrelated members need not wait on S3.
CANCEL has no reply and cancels only the identified pending request.
No byte-range lock request waits.

NEGOTIATE grants at least one credit, even when the request asks for zero.
SESSION_SETUP grants at least five credits, our chosen minimum.
The server grows the balance toward 256 within the bound in `features.go`.
The server replenishes credits before a valid synchronous compound can exhaust the client's balance.

## Protection and reconnect

SMB 3.1.1 uses SHA-512 preauth and NTLMv2 inside SPNEGO.
The server chooses only offered algorithms and prefers GMAC and AES-256-GCM.
Session-derived keys protect authenticated traffic.
Plaintext replies are signed, including final SESSION_SETUP.
Interim replies follow the protocol's unsigned-interim exception.
AES-GCM encrypts and authenticates encrypted traffic, including interim replies.
The server does not sign encrypted messages separately.
The server verifies the GCM tag before decoding plaintext.
A protector never reuses a send nonce.
Reconnect derives fresh keys and nonce state.

A transport drop detaches durable opens immediately, without waiting for S3.
The server cancels old request contexts but keeps acknowledged data and durable handles.
Detached opens retain granted access, sharing, deletion intent, locks and leases.
Non-durable opens close after active request references drain.
Old requests cannot publish grants or replies on the new binding.
DH2C validates the file ID, CreateGuid, client GUID, user, share and lease key.
The table assigns a new volatile ID when it attaches the open to the new session and tree.
The server accepts PreviousSessionId and reconnect offers containing only the old algorithms.
The server answers AAPL again on the new connection.
Expiry uses the normal close path.
Shutdown closes every attached and detached open and applies pending deletion.
An unmarked duplicate CREATE returns DUPLICATE_OBJECTID.
A marked replay must match the original identity, granted access and parameters.

On a sharing violation against an open with an H lease, the server breaks H, waits for the acknowledgment or timeout, and checks again.
A lease acknowledgment has no epoch field.
The table checks its identity and acknowledged subset against the current break.
Each returned break includes its captured current state and acknowledgment requirement.

A normal traffic drop of about 30 seconds can continue the same Mac backup.
An unanswered CREATE, LOCK or SET_INFO can make macOS refuse reconnect.
Longer drops fail the current backup visibly while earlier backups remain intact.
Connection drops keep acknowledged work.
Crashes with local disk intact keep flushed work.
Machine-loss recovery returns the last hourly metadata backup.
FULL_SYNC waits for S3 data, local metadata commit and a full-fsync barrier, not an hourly backup.

## Milestones and tests

Each issue closes with its own regression.
PR checks replay saved fuzz inputs.
Gates fuzz every target for one minute.
New packages run with the race detector and shuffled tests.
The manager copies these lists to the milestone issues and records later changes with their reasons.

### M1: core packages (#187)

Wire (#200):
- Each typed codec round-trips its declared layout.
- Malformed offsets and lengths return errors without a crash.
- A decoder rejects malformed UTF-16.
- A compound decoder validates the whole chain.
- FILETIME sentinels remain distinct from Unix epoch.
- Each decoder has a fuzz target.

Auth (#201) and crypt (#202):
- NTLMv2 matches MS-NLMP vectors.
- Invalid proofs and MICs fail authentication.
- SPNEGO advertises only NTLMSSP.
- The test initiator completes the vector exchange.
- Preauth and key derivation match MS-SMB2 vectors.
- CMAC and GMAC signatures match vectors under concurrent use.
- Both GCM key lengths reject a changed tag.
- Wrong-direction keys reject encrypted input.
- Concurrent sends never reuse a nonce.
- Cipher zero supports signing but refuses GCM operations.

State (#203):
- Share checks work in both directions.
- Failed reservations release their sharing intent.
- Append-only grants remain distinct from write-data grants.
- Metadata-only grants do not acquire data-read rights.
- Replay and reconnect preserve the full granted mask.
- Delete-pending rejects new opens.
- A stream's deletion and locks do not affect another stream.
- Unlock requires the exact owner and range.
- A lock vector applies atomically.
- Range overflow returns an error.
- Zero-byte ranges follow MS-SMB2 rules.
- Shared locks allow reads through the owning open.
- Shared locks allow reads through another open.
- Shared locks reject writes through the owning open.
- Shared locks reject writes through another open.
- Exclusive locks allow their owner's reads and writes.
- Exclusive locks reject another open's reads and writes.
- A free lock succeeds without FAIL_IMMEDIATELY.
- A conflicting lock fails immediately without FAIL_IMMEDIATELY.
- CloseTree closes durable opens as well as ordinary opens.
- Fake-clock expiry returns cleanup for detached opens.
- A valid acknowledgment succeeds after a nonzero notification epoch.
- A break from R to none captures its current state without requiring an acknowledgment.
- An RWH downgrade captures its current state and acknowledgment requirement.

Adapter (#268), using real file-backed JuiceFS:
- A flush through the non-writer handle commits the inode's writes.
- Upload errors reach the flushing caller.
- Metadata barrier errors reach the flushing caller.
- FULL_SYNC waits for the full-fsync barrier.
- A later flush cannot undo an acknowledged truncate.
- A later flush cannot overwrite an explicit timestamp.
- Lookup and directory entries include buffered length without flushing.
- Other inodes progress during a slow flush.
- Stream writes respect offsets and the 64 KiB limit.
- Storage primitives support the CREATE dispositions.
- Remove and base rename reject changed inode identities.
- Named-stream rename returns NOT_SUPPORTED without changing data.
- Read-only mode rejects mutations.
- Lookup of the empty path returns root attributes without a handle convention.
- StatFS reports configured capacity or the default free-space cap.

Raw client (#269), after #200 merges:
- Framing preserves supplied headers and credit fields.
- Pending and final replies correlate by MessageId and AsyncId.
- Raw send transmits malformed bytes unchanged.

S3 fault proxy (#271):
- Delayed headers and bodies obey the configured delay.
- Injected errors and throttling reach callers.
- Cut bodies terminate at the configured point.
- A timed outage ends at its configured deadline.

### M2: connection setup (#188)

LOGOFF and TREE_DISCONNECT tests put opens directly into the shared state table in `server.Options`, since M2 has no CREATE handler.

- The client performs login and message protection against the running server.
- smbclient authenticates and connects to the share without listing files.
- Opening SMB1 NEGOTIATE receives the wildcard response.
- Clients without SMB 3.1.1 are refused with a clear log.
- Required encryption refuses a client without GCM.
- Guest, Kerberos and IPC$ requests are refused.
- Final SESSION_SETUP verifies against the preauth transcript.
- Each plaintext compound member's signature is checked.
- Tampered GCM input is rejected before dispatch.
- Session and tree checks reject cross-session identifiers.
- ECHO works without file handlers.
- LOGOFF closes the session's opens.
- TREE_DISCONNECT closes the tree's opens.
- Initial and multi-credit grants cannot drain the balance to zero.
- Each producer receives its own sender completion.
- A partial write stops the sender.
- A controlled async error retains its identity and grants no final credits.
- ECHO compounds test framing without file operations.
- The real-adapter net.Pipe fuzz target always replies or closes within a bound.
- The credit allowlist contains only tests without file handlers.

### M3: file operations (#189)

- Two opens exercise every CREATE disposition, including supersede.
- GENERIC_ALL permits the expected data writes.
- CLOSE releases an open and returns selected-object attributes.
- READ sees acknowledged writes from another handle.
- WRITE_THROUGH waits for durable storage.
- Cross-handle FLUSH reaches S3 and the required metadata barrier.
- Truncate cannot resurrect old buffered bytes.
- SET_INFO handles timestamp sentinels without overflow.
- SET_INFO values -1 and -2 leave the stored time unchanged (#111).
- Later I/O updates times as usual after those sentinels, with no suppression state.
- Directory continuation reuses the original pattern.
- Directory matching treats literal characters and DOS wildcards correctly.
- Directory entries report live length.
- Base rename and delete preserve unrelated data.
- FsSize and FsFullSize encode their fields at the specified offsets.
- Unsupported classes and object-ID requests return a status before a successful ECHO.
- Delayed cold GET and PUT receive pending replies before completion.
- Final success and error retain async identity without duplicate credit grants.
- A five-minute S3 outage delays work without blocking unrelated requests.
- smbclient lists the directory successfully.
- The file-operation and compound allowlists pass.

The checked-in e2e subset contains TestSMBToS3Smoke, TestFilesystemOperations,
TestAuthentication, TestReadOnly, TestRecovery, TestMissingDataIsSMBError and
TestZeroCacheUnusableDirectory.
M3 enforces sharing reservations before destructive dispositions.
M4 adds the full sharing interoperability matrix.
M3 does not require streams, AAPL or leases.

### M4: Mac features (#190)

- Each stream disposition uses the selected stream's existence.
- Stream offsets, resize and size limits preserve the base file.
- Information queries and CLOSE return the selected stream's length.
- AAPL zero-byte stream opens return OBJECT_NAME_NOT_FOUND.
- Stream delete-on-close preserves the base and other streams.
- Disconnect applies pending deletion correctly.
- Stream locks protect only their selected stream.
- Deny-delete sharing prevents base rename.
- Delete-on-close checks delete access.
- Lock vectors never wait, with or without FAIL_IMMEDIATELY.
- CANCEL has no reply and affects only its pending request.
- CHANGE_NOTIFY returns NOT_SUPPORTED without delayed work.
- Every advertised filesystem bit has a passing test.
- The AAPL mask is exactly 0x06, with no resolve-ID bit.
- Case-sensitive lookup and rename match the advertised policy.
- Hard links, open-by-ID and sparse features receive no grant.
- Restart has no persisted JuiceFS plocks.
- The stream, sharing, create, deletion and non-blocking lock allowlists pass.
- TestResourceForkOffsetsAndResize passes.
- The Mac gate reads and writes xattrs and FinderInfo.
- The Mac gate creates and attaches a sparsebundle.
- The Mac gate verifies F_FULLFSYNC.
- The Mac gate completes a backup.

M4 still advertises no leasing and grants no durable handles.

### M5: leases and reconnect (#191)

- V2 leases grant only safe R, RH or RWH states.
- Conflicts produce the required breaks.
- A sharing violation against an H lease breaks H before a second share check.
- A valid acknowledgment succeeds after a nonzero notification epoch.
- An R-only break and an RWH downgrade carry the captured notification fields.
- Break timeout removes conflicting rights.
- Classic oplocks and directory leases receive no grant.
- Only regular unnamed files with H receive durable handles.
- Requested timeouts through 16 minutes are granted exactly.
- A larger request receives 16 minutes in the reply.
- A zero timeout receives 120 seconds.
- An unmarked duplicate CREATE fails.
- A valid replay reuses the original open without a mutation.
- DH2C rejects any mismatched reconnect identity.
- Disconnect preserves acknowledged data, sharing and ranges.
- New keys and volatile IDs reject stale traffic.
- Fake-clock expiry closes handles and applies pending deletion.
- The pure Go fault proxy drops connections during normal I/O.
- The lease and durable-v2 reopen allowlists pass.
- One counted Mac drop run completes the same backup after a cut during band writes.
- A longer drop fails visibly while earlier backups stay intact and the next backup succeeds.

A Mac refusal caused by an in-flight request counts as not tested and is rerun, up to three times.
Three refusals do not close M5; the manager decides the next step.
The tests use no kernel CIFS mount, privileged Docker or packet-filter commands.

## smbtorture policy

Each milestone checks in exact test names from a pinned Samba version.
Research names are candidates until the worker enumerates them.
A listed failure fails the gate.
Tests requiring unsupported features are not gates.
Full suites may run for information but do not replace the allowlist.
Every advertised capability needs a raw Go test or an allowlisted Samba test.
M6 runs the full e2e suite and accumulated allowlists before removing the old server.
