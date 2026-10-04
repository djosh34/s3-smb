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
name is source-verified but has no passing race-daemon proof here. `excluded`
means the whole test requires an unsupported expectation. A pending probe or
supported failure cannot become an exclusion merely to make a list pass.

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
establish a pass on a future dependency union. The staged list now contains
seven proven IDs.

## Probe blockers

All `probe` entries remain pending. In particular, the non-AAPL
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

Use the one shared `testSambaInterop(t, allowlist)` helper after its approved
extraction from [PR #433](https://github.com/djosh34/s3-smb/pull/433). Do not
merge that PR's M5 production activation merely to obtain the helper.
M5 [#348](https://github.com/djosh34/s3-smb/issues/348) remains a separate
selection and explicit activation.

The staged files are owned by #449. The M3 owner edits only the default list.
The combined-PR amendment on #176 puts the final selection in area F;
this staged branch is input to that combined PR. Keep it draft until
checked handlers and list activation are agreed.
