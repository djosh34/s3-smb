# M4 Samba selection

This is the staged selection for [M4 exact-name verification](https://github.com/djosh34/s3-smb/issues/449).
It does not change `smbtorture.allowlist`, activate M5, or enable named-stream
capabilities. The Mac feature owner controls handler and capability activation.
Supported failures block M4; they are not exclusions.

## Pinned enumeration

`testdata/smbtorture-m4.list` contains the 58 individual IDs in `smb2.streams`,
`smb2.sharemode`, `smb2.create` and `smb2.lock`, plus five tests deferred by the
M3 list owner: compound stream padding and the four parent-directory sharing
rename cases. There are no suite selectors or patterns.

The list was enumerated with the repository's pinned Docker image
`s3-smb-test:2e9c43ba8ee0436c3f9b0fc99707bf061bb898f6ddd72936b4ae5f54db95d7bb`:

```sh
docker run --rm --entrypoint sh "$image" \
  -c 'smbclient --version && smbtorture --version && smbtorture --list'
```

Both binaries reported `4.17.12-Debian`. The complete output, including both
version commands, had SHA-256
`95697e2c8414bd7e0718e28bcade9d57e637a03c4ad74d48067a9fd3e298d2ae`.
The checked-in list is only the owned subset, not the complete tool listing.

Requirements and exclusions come from Samba's
[`samba-4.17.12` source](https://github.com/samba-team/samba/tree/samba-4.17.12/source4/torture/smb2).
`smbtorture.m4.inventory` records each exact ID, its state and the source lines.
The source files used for the four families have these SHA-256 hashes:

| File | SHA-256 |
| --- | --- |
| `create.c` | `967b4e582ee39d45f327a2a5db389e1e6ab5839f2ff2fbdde47894afce60f016` |
| `lock.c` | `e18443c0a3f4a31d3f39c280cb406f967c99ee1d433304b8a1d357cad00ba9db` |
| `sharemode.c` | `aeb39c45c017e4627238cbb56a6482063bc8d1d545b6589bf8ea03af56dcfd7e` |
| `streams.c` | `01d95f8066c566816903f2d5b4733ab76bb74cbd8774b0803dee127f80b8f5dd` |

`selected` means passing evidence is recorded below. `probe` means the exact
name is source-verified but unproven. `blocked` means a supported expectation
failed. `excluded` means the whole test requires an unsupported expectation.
A pending probe or supported failure cannot become an exclusion merely to
make a list pass.

## Passing evidence

[PR #415](https://github.com/djosh34/s3-smb/pull/415) at
`bc71cfdef9ca013192b80ff1c741a3e9ff177aa1` reports three passing pinned IDs:

- `smb2.sharemode.sharemode-access.sharemode-access`
- `smb2.sharemode.access-sharemode.access-sharemode`
- `smb2.create.multi.multi`

That run used the in-process server with the real storage adapter and encrypted
SMB. The race-enabled wrapper passed on its second run. The first run reported
a JuiceFS root GetAttr timeout race even though Samba reported success. It was
reported to the Mac lead, not dismissed as a Samba exclusion. Sharing delta
`92097d1c6bf55021ed341d9db0a0fd8afcb00c2b` preserves this earlier evidence;
it is not a new probe.

Four new real daemon probes passed on the assigned checked Mac-area LOCK stack,
[PR #389](https://github.com/djosh34/s3-smb/pull/389) at
`a2e42e77a5c529a3853298c3a3d17fed289d97d3`:

- `smb2.lock.rw-shared.rw-shared`
- `smb2.lock.rw-exclusive.rw-exclusive`
- `smb2.lock.auto-unlock.auto-unlock`
- `smb2.lock.lock.lock`

The detached checkout was clean. It already registered CREATE, CLOSE, READ,
WRITE and LOCK; no handler or dispatch edits were made. The Linux arm64
executable was built with Go 1.26.3, `-race -tags smbnext`, and run with
`GORACE=halt_on_error=1` against an isolated real-adapter MinIO bucket.
Build metadata was checked before the probes. Samba authenticated with
encryption and SMB 3.1.1. Each exact ID reported subunit success and exit 0.
The daemon then exited 0 after SIGTERM with no race output.

`testdata/smbtorture-m4-lock.proof` retains raw output, Samba seeds,
source and executable hashes, commands and the local artifact path. This
probes the Mac area's checked stack, not a native macOS gate. It does not
establish a pass on a future dependency union.

A second clean race-daemon run on the same LOCK source tested the other 12
source candidates. Nine passed. Two supported expectations failed and remain
blockers. `multiple-unlock` failed because it requires partial unlock commit;
that expectation is excluded by the final M0 atomic-vector contract and
[the existing LOCK triage](https://github.com/djosh34/s3-smb/pull/389#issuecomment-5979338429).
This is an explicit design exclusion, not a request to change unlock behavior.
Raw output for all 12 tests is in `testdata/smbtorture-m4-lock-more/`.
Samba seeds were 1791117958 through 1791117960. Daemon exit was 0 with no race;
raw stderr SHA-256 is
`89b0e135440e72b7f230d8898911440e2c18fba20ae5257e63de67ede3785496`.

Nine CREATE cases ran against the M3 owner's unchanged approved local union
`6578d39b2ce953037bbaf3728fa27a2bf54a8773`. It contains checked CREATE/CLOSE,
FLUSH, READ/WRITE, directory and information handlers, but no LOCK registration.
Its inputs were #376 `1f1b5d4` (including #380 `f0c1353`, #399 `0e5d912`,
#410 `365c907`), #395 `f2680fe`, #398 `4064e5b`, #397 `7252f34`,
#374 `e137e2b` and #404 `1e7f4e3`. No source or policy edits were made for
these probes. The race smbnext executable SHA-256 is
`10780fe2107bcfbb0bd64e1cf18a179ca3b2b9d149b89b18e6f5c055417072d4`.
Seven passed, including a fresh `multi` pass; two supported status checks failed.
Raw output is in `testdata/smbtorture-m4-create/`, with artifacts at
`/tmp/449-evidence/create-6578d39`. The same pinned encrypted command and
isolated MinIO procedure were used. Daemon exit was 0 with no race; raw stderr
SHA-256 is `55c0035efc14b392e927b49b26b06ea167f9a5bee5b72478a785d91ab43d14bc`.

`smb2.create.brlocked.brlocked` separately passed on the unchanged checked
LOCK stack `a2e42e77`, which already registers LOCK. Its raw CREATE output
is also in `testdata/smbtorture-m4-create/`; Samba seed was 1791119116,
with clean daemon exit 0 and no race output. It was not run on the M3 union,
which lacks the required LOCK handler.

These external daemon probes used a one-hour metadata-backup interval.
The canonical shared fixture uses two seconds. M3's canonical probes exposed
a metadata-backup/Load race under that workload, reported to P1 and test-tools.
A clean external probe does not establish a clean canonical fixture or gate.
The complete selected M4 list still needs that checked-stack rerun.

The staged selection contains 23 proven IDs. This is not a green M4 gate:
passing subsets do not erase the supported blockers below.

## Probe blockers

| Exact ID | Observed failure | Owner |
| --- | --- | --- |
| `smb2.lock.valid-request.valid-request` | `lock.c:281`: mixed FAIL_IMMEDIATELY flags returned OK instead of INVALID_PARAMETER | Combined E LOCK/state |
| `smb2.lock.zerobyteread.zerobyteread` | `lock.c:1529`: zero-byte READ returned FILE_LOCK_CONFLICT instead of OK | Combined B READ |
| `smb2.create.leading-slash.leading-slash` | `create.c:1514`: OBJECT_NAME_INVALID instead of INVALID_PARAMETER | Combined B CREATE |
| `smb2.create.impersonation.impersonation` | `create.c:1558`: OK instead of BAD_IMPERSONATION_LEVEL | Combined B CREATE |

No handler fixes are owned by this selection work. All `probe` entries remain pending. In particular, the non-AAPL
zero-byte enumeration test is not proof of AAPL zero-byte-open behavior.
The base-rename-with-open-stream test expects ACCESS_DENIED even though the
stream shares deletion; the Mac owner must compare that expectation with the
M0 rename contract before activation. Stream spelling and base-overwrite
expectations also need owner review, not silent server changes.

A fresh probe needs an owner-approved checked Mac handler union, a daemon built
with `-race -tags smbnext`, pinned tool versions and enumeration, encrypted
SMB 3.1.1, exact subunit success lines, and clean daemon shutdown/race output.
Record the daemon source SHA, all dependency heads, command, shuffle seed,
per-ID result and artifact location. A source inspection or mocked runner test
is not passing interoperability evidence.

`TestSambaM4Interop` uses the one shared Samba fixture helper, whose test-only
extraction was approved from [PR #433](https://github.com/djosh34/s3-smb/pull/433).
The default integration command still selects only `TestSambaInterop` and its
unchanged default list. Run M4 explicitly only on an agreed feature stack.
Do not merge M5 production activation merely to obtain the helper.
M5 [#348](https://github.com/djosh34/s3-smb/issues/348) remains a separate
selection and explicit activation.

The checked client-command seam is reused byte-for-byte from the M3 owner's
local commit `7f061e141b11856dc0786a715b495349e4c169a6`.
M2 and M4 explicitly use `quit`; M3's empty-root `ls` check stays separate.
No helper override or duplicate fixture was added.

The staged files are owned by #449. The M3 owner edits only the default list.
The combined-PR amendment on #176 puts the final selection in area F;
this staged branch is input to that combined PR. Keep it draft until
checked handlers and list activation are agreed.
