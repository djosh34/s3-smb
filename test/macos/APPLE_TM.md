# Apple Time Machine capability trial

Manual `macos.yml` on this branch runs exactly one `macos-15-intel` job. No retry,
restore, recovery, file-copy substitute, induced pressure or protocol manipulation.
Dispatch requires coordinator approval of the exact revision and a Mac allocation.

## Ordinary setup

Reuse `Acceptance.platform`, exclusions, `configure_destination`, seeded `create_tree`
(`random.Random(64)`, 4,000,000 random bytes plus the historical small tree),
`start_backup`, and completed-backup selection/tree-presence checks. First assert
no existing backup is running. System/kernel/client security and signing are unchanged.

The installed `smbd -help` must document `-ports`. Start our own `/usr/sbin/smbd
-ports 1445`, require the owned process stays alive and the direct IPv4 loopback
listener becomes available. No PF/launchd/relay fallback. Provision a disposable
local server account using historical ordinary SMB credential setup; all account
credentials, command output and exceptions remain private. Unsupported direct
listener operation is a capability failure, not an s3-smb finding.

Create **only** an outer 1 PiB virtual HFS+J sparsebundle with ordinary `hdiutil`;
mount at an owned path and share an initially empty directory. Backupd creates its
own fresh inner bundle over SMB. No inner plist edits, band overrides, preformatting,
sparse-support changes or synthetic zeroing. The outer filesystem is intentionally
different from the historical Apple directory on APFS, and may affect pressure.

Documented sparse virtual capacity is not allocated or reserved physical storage.
HFS+ sparse-image free space may be capped by physical host space; actual local and
SMB `statvfs` numbers are recorded, not inferred. Runtime `Info.plist` measurement
of the backupd-created inner bundle must establish its exact band geometry.
8 GiB = 8,589,934,592 bytes. Both historical controls had 16 TB inner images;
only that size is insufficient to establish equal formatting exposure.

## Physical resource bounds

After tooling, require at least 96 GiB actual free storage. Plan: backing24GiB,
raw24GiB, private logs1GiB, ciphertext26GiB (including tar/codec allowance), reserve20GiB:
95GiB total,1GiB admission slack. Outer creation has a five-minute bound and a
physical allocation guard. Backup has a fifteen-minute bound; capture max20minutes,
raw24GiB and free floor46GiB.
Check backing allocation/log size/free space every5s. These are safety aborts,
never intentional pressure. Do not shrink guards to make admission pass.

Before encryption check fresh actual free space for raw+logs+21GiB. Preserve all
originals; no storage deletion. Local downloads require separate resource admission;
24GiB raw/26GiB cipher upper bounds do not fit current local available storage.

## Observation and retention

Shared numeric sampler every5s, known owned PIDs only. TM status/local inner
geometry at before/progress60s/after-command. Same source fixture and baseline
cadence as rc7. Public metadata contains integers/booleans/fixed labels only.
Actual WRITE sizes/concurrency/signing/receive windows require authenticated
independent capture analysis; no values invented from band size or `statvfs`.

Passive helper starts before the first SMB mount, requests32MiB and reports the
actual BPF buffer before readiness. Stop after ordinary backup/SMB teardown,
SIGINT drain then wait<=10s. Full snaplen/both directions; no Python raw parsing
on the Mac. Helper capture health does not prove stream completeness. The first
trial is explicitly unmatched until actual outputs are reviewed.

Freeze separate command exit, new-completed selection, tree presence, capability,
cleanup, sampler, capture health, client log parse/enum and authenticated retention
outcomes. No restore/hash/recovery claim. Native logs are queried only for this
single attempt, with2s wall-clock margin on normal completion. On any guard/abort,
freeze the end wall/monotonic boundary before cleanup, mark harness_aborted, and
use no end margin. Known clock adjustments remain explicit.

Reviewed public-certificate-only CMS AES-GCM retention runs **before any offline
analysis**, including logs-only retention for early setup failures. Private directory
0700/files0600; only the publication directory is uploaded. No key on the runner,
no raw or private-log fallback. The first trial is one independent run; any later
repetitions require new allocation/review and retain every earlier mismatch/failure.

## Sources and limits

- Historical working Apple setup: `ea5314c:test/macos/acceptance.py:532–590`, issue45.
- `smbd -ports`: https://keith.github.io/xcode-man-pages/smbd.8.html (old manual;
  installed help is mandatory, not inferred support).
- Sparse virtual size and HFS+ backing free semantics:
  https://www.manpagez.com/man/1/hdiutil/osx-10.12.3.php and
  https://keith.github.io/xcode-man-pages/hdiutil.1.html . Retain installed manual.
- Matching criteria: `/tmp/s3-smb-swarm-64-takeover/workload-match/acceptance-table.md`.

No Darwin trial has run at preparation time. Successful ordinary Apple work,
particularly with faster formatting or different signing/pressure, cannot exonerate
s3-smb, prove recovery, or explain the historical short-header error.
