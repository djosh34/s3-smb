# M5 candidate results

Activation is blocked. All six compatible Samba candidates ran and failed.
No candidate moved to the exclusions, and no server or Samba policy changed.
The PR stays draft, and LEASING is disabled again after the private probe.
This evidence closes neither #348 nor #138. The latter also requires the
filesystem and Mac evidence owned by #122.

## Candidate and checks

Tested head: `034617a7351d8c6bda8ff14f28b3041e6c434b02` on
`plan/348-leasing-allowlist` (#433). Canonical sources:

- Durable grant/replay/reconnect #427: `4de26e7`.
- Lease breaks #401: `b18e318`.
- Scavenger #375: `1c80c60`.
- Network reconnect tests #418: `b8d52d2`.
- CREATE leases #424: `81f1694`.
- Integration and existing Samba runner #333: `82042f0`.

Handler registration conflicts retained only the owners' command lines.
No connection, protection, lease, durable, timeout or storage behavior was
adapted. The private tested candidate enabled the LEASING bit after the complete raw
Go feature proof passed. Its mask was exactly LARGE_MTU | LEASING (0x06), with
filesystem and AAPL masks unchanged. The failed probe did not permit activation:
LEASING and the exact-mask expectations were reverted to 0x04. Enabling the bit
and asserting 0x06 is the final step only after the full checked dependency
stack, raw Go proof and all six compatible Samba tests pass.

Race/shuffle tests passed for smb, server, state, smbtest and test/e2e. This
includes the in-process real-adapter lease/durable/replay tests, network-cut
reconnect tests, expiry and pending deletion. Fuzz seeds, scoped vet and pinned
golangci-lint 2.14.0 passed. The exact feature-mask regression failed before the
bit change; the three wire-mask expectations were updated for the private probe without
weakening them to contains-bit checks. They now assert the disabled 0x04 mask.

## Samba run

Samba: `4.17.12-Debian`. The inventory still contains exactly 56 names: six
compatible candidates and 50 source-backed exclusions. The M2 list is unchanged.
The existing runner performs version/list validation, encrypted SMB 3.1.1 login
and exact-name success checks. `TestSambaM5Interop` calls the shared
`testSambaInterop` fixture separately for each exact candidate, so one failure
does not prevent the others from running.

The daemon was built with `-race -tags smbnext -buildvcs=false`. The test binary
was built with `go test -race -c ./test/e2e`. Both ran with MinIO in one pinned
Linux test container, as UID 501/GID 1000, with `--network none`, `--cap-drop ALL`
and `no-new-privileges`. No kernel CIFS, privileged container, packet filter,
Mac run, full local check or test lock was used.

The complete run on 2026-10-04 took 54.40 seconds and returned exit status 1:
0 passes, 6 failures, 0 skips, 0 missing tests. Go shuffle seed:
`1791117087621419317`. Each full name below has a corresponding test and failure
record. Source locations refer to the pinned Samba `source4/torture/smb2/lease.c`.

| Exact candidate | Result | Fatal assertion and earlier diagnostics | Minimal classification |
| --- | --- | --- | --- |
| `smb2.lease.v2_epoch1.v2_epoch1` | FAIL | [Line 1796](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L1796): file attributes 0x80, expected 0x20 (ARCHIVE). Same mismatch at line 1782. | Other area: CREATE/adapter attributes. Routed to files-io. |
| `smb2.lease.v2_breaking3.v2_breaking3` | FAIL | [Line 2587](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L2587): NT_STATUS_UNSUCCESSFUL, expected OK. Earlier diagnostics report H instead of RH, an extra break, lost shared grant, epoch/flags differences and attributes 0x80 instead of 0x20. | M5 grant/epoch/break/ACK behavior, plus other-area attributes. |
| `smb2.lease.v2_complex1.v2_complex1` | FAIL | [Line 3469](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L3469): expected lease-break transport, got none. Earlier diagnostics report missing same-key grants, shared-state/epoch differences, H instead of RH and attributes 0x80 instead of 0x20. | M5 grant/epoch/WRITE break behavior, plus other-area attributes. |
| `smb2.lease.v2_complex2.v2_complex2` | FAIL | [Line 3571](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L3571): cross-login acknowledgment returned INVALID_PARAMETER instead of OK. The captured break target was H rather than RH. Attributes also differed. | M5 break target/ACK behavior. The RH acknowledgment exceeds the captured H target; this is not yet an isolated cross-login identity bug. |
| `smb2.lease.v2_rename.v2_rename` | FAIL | [Line 3925](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L3925): rename returned NOT_SUPPORTED instead of OK. Attributes also differed. | Missing M3/M4 dependency: the candidate has no SET_INFO registration. Not a diagnosed rename defect. |
| `smb2.lease.v2_bug15148.v2_bug15148` | FAIL | [Line 4749](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L4749): expected lease-break transport, got none. Earlier diagnostics report missing R grant, wrong notification identity/epoch, an extra break and attributes 0x80 instead of 0x20. | M5 grant/epoch/WRITE break behavior, plus other-area attributes. |

The attribute request is `CREATE lease_v2_epoch1.dat`, OPEN_IF,
SEC_RIGHTS_FILE_ALL, sharing RWD, FILE_ATTRIBUTE_NORMAL (0x80), regular unnamed
file, and RqLs V2 RWH with epoch 0x4711, then 0x11. The
[generic/V2 helpers](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/util.c#L882-L975)
set those fields. The response succeeds with action CREATED but reports 0x80;
[CHECK_CREATED at lines 1782/1796](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/lease.c#L1773-L1797)
requires ARCHIVE (0x20). This exact seam failure was sent to the files-io lead;
no attribute patch was made here.

The rename classification was checked against the actual candidate
`internal/smb/server/handlers.go`, which has no SET_INFO entry. Canonical #336
at `1e7f4e3` registers handleSetInfo and implements setRenameInfo. Canonical #111
at `4064e5b` supplies basic SET_INFO but explicitly leaves rename to #336.
Neither metadata source is in this candidate. The failure remains a blocker
pending a checked dependency import and rerun, not an exclusion.

These are observations, not a completed root-cause diagnosis. The leases lead
received the full table and retains all six failures as blockers. Attribute,
CREATE lease-state/epoch, WRITE break, cross-login acknowledgment and rename
failures need their owners' triage. In particular, this run is not authority to
weaken the recorded conservative CREATE-break decision or to exclude a
compatible test.

Local evidence: `/tmp/owner348/evidence/samba-m5-results.log`,
`samba-m5-exit-code.txt` and `daemon-logs/`. The earlier interrupted run is saved
separately as `samba-m5-interrupted.log` and is not counted as complete evidence.
The reproducible container invocation is in `run-probe.sh` with the inner
`run-container.sh`; the selected test is exactly `^TestSambaM5Interop$`.

The durable-v2 coverage gap and immutable timeout policy remain recorded in
[the capability inventory](smb-m5-capabilities.md). Raw Go success is not a
passing Samba gate, and a green default CI run would not resolve these explicit
M5 probe failures.
