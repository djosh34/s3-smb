# M5 leasing and durable-handle gates

Preparation for [#348](https://github.com/djosh34/s3-smb/issues/348) and the
leases-area part of [#138](https://github.com/djosh34/s3-smb/issues/138).
The production LEASING bit remains disabled. None of the candidate Samba tests
has passed against smbnext in this preparation branch.

## Activation

`test/e2e/smbtorture.allowlist` remains the runner's selected M2 list.
`test/e2e/smbtorture.m5.allowlist` is a separate candidate list. The inventory
and exclusions are not executable test selectors. Do not select the M5 list
or enable LEASING until grants, breaks, durability and
[#347 reconnect tests](https://github.com/djosh34/s3-smb/issues/347) pass.
Then run every selected exact name against the pinned client and the
race-enabled smbnext daemon. A skip or missing success report is not a pass.

The final activation must preserve the other milestone lists. It must also
check that NEGOTIATE advertises exactly LARGE_MTU | LEASING (0x06), without
DFS, multichannel, persistent handles or directory leasing. The existing
`internal/smb/features_test.go:TestFeatureMasks` currently expects 0x04;
update that test only when the bit is enabled and add a wire-level assertion.

## Pinned enumeration

Pin: Samba `4.17.12-Debian`, Debian packages
`2:4.17.12+dfsg-0+deb12u4`, from `test/Dockerfile` and runner PR
[#333](https://github.com/djosh34/s3-smb/pull/333).
On 2026-10-04, `smbtorture --version` and `smbtorture --list` were run in the
pinned image with networking disabled. The 56 individual names in the lease,
durable-v2-open and durable-v2-delay families are saved in
`test/e2e/testdata/smbtorture-m5.list`. Tests require each name to appear once
in either the six candidates or the 50 explicit exclusions. Wildcards and
suite selectors are never accepted.

Primary sources are the Samba `samba-4.17.12` tag:

- [lease.c](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c)
- [durable_v2_open.c](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/durable_v2_open.c)
- [util.c helpers](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/util.c#L910-L990)

The helpers distinguish V1 (`lease_request`) from V2 (`lease_request_v2`).
A test name containing `v2` alone is not enough to establish its scope.

## Lease candidates

Each name below has the prefix `smb2.lease.` and repeats its individual name
as the final component. The allowlist stores the full IDs, for example
`smb2.lease.v2_epoch1.v2_epoch1`.

| Individual name | Primary-source scope |
| --- | --- |
| `v2_epoch1` | Initial lease epoch on an unnamed regular file. [Lines 1741-1804](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L1741-L1804). |
| `v2_breaking3` | Pending CREATEs, shared-key opens during a break, captured epochs, acknowledgment and chained downgrades. Requested classic oplock level is NONE, not an oplock grant. [Lines 2460-2664](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L2460-L2664). |
| `v2_complex1` | Independent logins with one client GUID, lease upgrade, shared epoch and breaks during writes. These are separate connections, not multichannel binding. [Lines 3337-3482](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L3337-L3482). |
| `v2_complex2` | A break acknowledgment on a second login with the same client GUID. [Lines 3484-3593](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L3484-L3593). |
| `v2_rename` | Rename preserves the initiating lease and breaks another lease. [Lines 3867-4000](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L3867-L4000). |
| `v2_bug15148` | R-only lease breaks on writes, with no duplicate notification on the next write. [Lines 4654-4761](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L4654-L4761). |

These are scope candidates, not proof that current handlers match Samba's
ordering. WRITE/rename breaks and cross-login acknowledgment need real runs.
An in-scope failure must be routed to its handler owner, not removed from the
list just to make a gate pass.

## Exclusions

`test/e2e/smbtorture.m5.exclusions` records every excluded full ID with one of
these reason keys. One unsupported prerequisite is enough to exclude a case;
the table notes additional prerequisites where they matter.

| Reason key | Reason and primary source |
| --- | --- |
| `lease-v1` | Requires a V1 lease grant. Only file leases V2 are implemented. Plain lease helpers set `lease_request`, not `lease_request_v2` ([util.c lines 910-975](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/util.c#L910-L975)). `v2_epoch2` and `v2_epoch3` explicitly mix V1 and V2 ([lease.c lines 1806-2012](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L1806-L2012)). Durable `open-lease`, `reopen1a-lease`, `reopen2-lease` and delay-msec also use the V1 helper ([durable_v2_open.c lines 405-511, 753-890, 1251-1298, 2269-2330](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/durable_v2_open.c)). |
| `directory-leases` | `v2_request_parent` and `v2_request` require DIRECTORY_LEASING and skip without it. They request directory leases, which this server refuses ([lease.c lines 1458-1511, 1586-1739](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L1458-L1739)). |
| `rw-break` | `break_twice` uses V2, but requires RWH -> RW -> R. The supported lease states are NONE, R, RH and RWH, not RW ([lease.c lines 1513-1584](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L1513-L1584), decision [#169](https://github.com/djosh34/s3-smb/issues/169)). |
| `oplocks` | Requires classic batch/level-II/exclusive oplock grants. The server grants none and grants durability only with an H lease. `create-blob` also uses durable-v1 contexts in its malformed-combination checks. See [durable_v2_open.c lines 87-405, 514-752, 927-1249, 2182-2267](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/durable_v2_open.c). |
| `timeout-policy` | Both V2-lease cases request UINT32_MAX and assert 300000 ms. The immutable [#169 timeout contract](https://github.com/djosh34/s3-smb/issues/169) requires 960000 ms. `reopen2-lease-v2` is an actual DH2C path, not a V1 test ([lines 1497-1550](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/durable_v2_open.c#L1497-L1550)); `durable-v2-setinfo` has the same mismatch ([lines 2021-2075](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/durable_v2_open.c#L2021-L2075)). |
| `app-instance` | Requires AppInstanceId fencing, which is not an M5 contract, and begins with batch oplock durability ([durable_v2_open.c lines 1746-1887](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/durable_v2_open.c#L1746-L1887)). |
| `persistence` | Exercises the persistent-handle/CA-share matrix with classic oplocks or V1 leases. Neither classic oplock nor V1 lease grants are supported; persistent handles are also refused ([durable_v2_open.c lines 1889-2019](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/durable_v2_open.c#L1889-L2019)). |
| `dynamic-shares` | Requires a preconfigured Samba dynamic share whose path changes between logins, plus V1 lease grants. This server has one fixed disk share ([lease.c lines 4002-4088](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L4002-L4088)). |

The durable-v1 `smb2.durable-open` family is outside the M5 gate. Blocking-lock,
classic-oplock, directory-lease and persistent-handle families are not selected.
The replay suite is also not selected wholesale: its durable replay cases use
classic oplocks/V1 leases or the same 300000 ms expectation
([replay.c](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/replay.c)).
The Go replay tests remain necessary; the server must not add unsupported grants
or change its timeout policy to satisfy a Samba test.

## Durable reconnect coverage gap

There is no eligible unchanged durable-v2 test in the two pinned durable-v2
families. This is not a passing durable reconnect gate, and not authority to
amend [M5's test list](https://github.com/djosh34/s3-smb/issues/191). The lead
must record a coverage-gap decision before M5 can close. Proposed evidence is
[#346's raw DH2C/replay tests](https://github.com/djosh34/s3-smb/issues/346) and
[#347's real-adapter network-fault tests](https://github.com/djosh34/s3-smb/issues/347).
Keep the pinned runner and server policy unchanged.

## Capability and grant evidence

This table covers only the leases area. Filesystem attributes and AAPL are
owned by the mac area through [#122](https://github.com/djosh34/s3-smb/issues/122).
References to open component PRs are planned evidence, not claims that the
integration branch already passes them.

| Wire surface | Intended M5 behavior | Evidence to require before activation |
| --- | --- | --- |
| NEGOTIATE LEASING bit 0x02 | Off in this prep branch; final mask 0x06 with LARGE_MTU. No new directory/persistent/multichannel/DFS bits. | Exact-mask tests in `internal/smb/features_test.go` and a wire NEGOTIATE assertion in #348, plus lease candidates after #347. |
| CREATE oplock level 0xff and `RqLs` V2 | Only regular unnamed files grant safe R (1), RH (3), RWH (7). Classic oplocks, V1 leases, directories and named streams receive no grant. | #345 `TestCreateLeaseV2SupportedStates`, `TestCreateLeaseSafeSubsetWithOtherOpens`, `TestCreateRefusesClassicOplocksAndNonFileLeases`, `TestCreateLeaseContextValidation`. |
| `RqLs` key, state, epoch and flags | Echo the lease identity; shared opens use table state/epoch. BREAK_IN_PROGRESS is captured during a pending break. PARENT_LEASE_KEY_SET reports a supplied parent key, not a directory grant. | #345 `TestCreateLeaseSharesStateEpochAndParent`, `TestCreateLeaseResponseWithoutParentFlagClearsParent`, `TestCreateLeaseResponseCarriesPendingSharedBreak`; state `TestPrepareLeaseSharedStateAndEpoch`. |
| OPLOCK_BREAK lease notification and acknowledgment reply | Captured current/target state, epoch and acknowledgment flag. Plaintext notification unsigned, encrypted notification GCM; acknowledgment carries no epoch. | #343 `TestLeaseBreakNotificationAndAcknowledgment`, `TestLeaseBreakAcknowledgmentRejectsInvalidRequests`, `TestDetachedLeaseBreakRetainingHCompletesWithoutNotification`; #396 unsigned-notification tests. |
| `DH2Q` CREATE reply | Durable-v2 only with H on a regular unnamed file. No persistent flag. Requested timeout granted exactly through 960000 ms; zero gets 120000 ms. | #346 `TestDurableTimeoutsAndPersistence`, `TestDurableGrantRequiresUnnamedRegularFileAndH`. |
| `DH2C` CREATE result | Retained lease, sharing, ranges and data. Same persistent FileID, fresh volatile FileID and new session keys. Validate CreateGuid, client GUID, user, share and lease key. | #346 `TestDH2CReattachesRetainedOpen`, `TestDH2CRejectsMismatchedContextsAndIdentities`, `TestDH2CRejectsOtherUserShareAndClient`; #347 `TestDurableReconnectDuringIO`, `TestDurableReconnectExpiresAfterNetworkCut`. |
| CREATE replay flag | Marked matching replay reuses its open without repeating mutations; an unmarked duplicate fails. Not a separate NEGOTIATE capability. | #346 `TestDurableReplayNeverRepeatsMutations`, `TestDurableReplayRejectsChangedParameters`, `TestReplayContextOrderDoesNotChangeParameters`. |
| Expired durable/break state | Whole lease revoked on timeout; detached opens close with pending deletion. No grant survives expiry or shutdown. | #344 fake-clock expiry tests and `TestScavengerExpiresWhileCleanupIsBlocked`; #347 network-cut expiry test. |
