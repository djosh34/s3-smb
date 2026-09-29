# Final hosted-Mac acceptance (#34)

**Implementation, not executed Mac evidence.** Linux at
`139b80da357f84c21bb83eb79a863b8545e2c800` passed local/identical CI 108/108 and
public fresh-cache `v0.1.0-rc.1` installation. That prerelease remains Mac-pending.
Do not close #34/#18 or advertise Time Machine compatibility from helper tests.
The [contract](implementation-plan.md) and [research](research/github-macos-time-machine.md)
remain authoritative. The research is not executed CLI proof.

## Ownership and execution gate

`.github/workflows/macos.yml` is manual and final, using the hosted `macos-15`
VM itself. Dispatch only after a clean immutable-snapshot review handoff. Inputs
are the reviewed full SHA and an immutable published version at **that same SHA**.
The integration owner handles publication, dispatch, downloaded artifacts and
narrow fixes. No nested VM, Docker assumption, public listeners, paid resources,
permission-control modification or early capability-probe workflow is used.

```sh
# Only AFTER clean review/handoff, publication and any required earlier gates:
gh workflow run macos.yml --ref acceptance/hosted-mac \
  -f reviewed_sha=FULL_REVIEWED_SHA -f public_version=vX.Y.Z-rc.N
```

A newly introduced workflow must first be present on the repository's default
branch for GitHub manual dispatch. Publish the reviewed snapshot without quietly
changing its SHA. Any application fix discovered here requires the full local
Linux suite, identical Linux CI and fresh public installation again before Mac
rerun. The green Linux entrypoint/ledger is unchanged.

## Sequence and native boundaries

`test/macos/run.sh` first builds with native Darwin SDK/CGo and Go **1.26.3**,
then performs the real public `go install` in an empty external directory with
fresh module/build caches, public proxy/checksum service and no workspace. The
installed public binary is the executable under test. MinIO is compiled natively
from Linux-matching commit `0d7408fc9969caf07de6a8c3a84f9fbb10a6739e`.

`acceptance.py` records installed `man tmutil`, per-verb help, platform/image,
service, mount and disk information. It checks exact options before using them.
Service enable/bootstrap is ordinary administration of the existing Apple
service if it is absent; it is not a Full Disk Access grant. Actual native
commands must succeed. Permission denial, unexpected CLI/schema/layout,
unattended consent, missing source access or command timeout fails visibly.
There is no privacy database edit, synthetic image, fallback copy, skipped gate
or `continue-on-error` acceptance path. Diagnostic commands alone may fail,
with their status/output explicitly retained.

1. Measure source/storage; establish native MinIO on loopback19000, observation
   proxy19001/control19002, and application SMB127.0.0.1:445. The initially empty
   bucket/share must really be empty. `mount_smbfs` and Time Machine both use the
   named `timemachine` account with an explicit empty password, not guest access.
2. Back up the full normally eligible runner with Apple's Time Machine. Do not
   limit its source to proof files. Require blocking-command completion, inactive
   status and a new completed backup in the **remote** image. Record a complete
   pre-wipe baseline manifest and source-namespace coverage.
3. Separately wait for a successful native application receipt whose snapshot
   starts after Time Machine completion. Leave the **default hourly interval and
   14-day trash policy** unchanged. Ordinary intact restart must reuse that
   receipt and demonstrate its remote GET/readback before the authority is wiped.
4. Detach image/SMB mounts, stop the daemon, remove its entire task-owned local
   directory (config/state/SQLite/WAL/cache/local keys), and recreate only the
   documented S3/volume/passphrase/SMB inputs. Recovery uses the application's
   normal selected point and PTY confirmation; exact prompts/point are retained.
5. Reattach the network sparsebundle read-only. `tmutil listbackups/latestbackup`
   select the remote completed backup. `diskutil` proves its parent device is the
   attached image, not the source disk/local APFS snapshots. Restore **every
   complete backed-up volume root** with `tmutil restore`, into a new empty
   task-owned output. Compare complete manifests, not just proof files.
6. Change/add/delete ordinary source proof files as a reproducible supplement.
   Start a later real backup. The fixture holds a successful actual chunk PUT
   **response**: upstream already committed, daemon request still pending.
   Require upstream2xx/pending request identity and active Time Machine status,
   recheck the hold, then SIGKILL only the application. This matches the Linux
   boundary; it does not claim an uncommitted object was lost. Stop the client,
   detach stale mounts, wipe local daemon data again, recover normal policy, and
   restore/compare the complete original baseline again.
7. Resume Time Machine, capture its different completed identifier and **complete
   independent manifest before wiping**, then require the subsequent native point.
   Cold-recover again and require that exact completed identifier, with its entire
   post-recovery backup manifest equal to the pre-wipe one. Perform another full
   native remote restore, including ordinary contents and changed/added/deleted
   proof-file state. Expectations are never derived solely from recovered data.

The application kill is not VM power loss, MinIO disk failure or S3 storage loss.
No object/backup repair or handpicked older application recovery point is used.

## Source, exclusions and capacity

Only two task-owned paths are added to native exclusions, listed with reasons in
`test-exclusions.json`: backend/daemon/mount/restore infrastructure, and growing
backup-test evidence. **Native/public/MinIO builds, their source/module/build
caches, checkout, SDKs and ordinary user files remain outside those exclusions.**
No ordinary source content is deleted to fit. Apple's own native exclusions
remain in effect and are recorded, not overridden with a fixture-only source.

The source inventory asks installed `tmutil isexcluded` for every encountered
entry, pruning only excluded trees. Denied/incomplete enumeration is a failure,
not zero bytes. Additional eligible mounted filesystems require explicit complete
coverage; an unexpected one fails rather than being silently omitted. Full raw
native inclusion output and the included/excluded path inventory are retained.
Preflight included paths must be present in the completed baseline Data volume;
live-source removals are reported, not silently waived.

Record logical lengths, allocated blocks and unique-hardlink versions of both,
as well as actual filesystem free space. These are different observations:
APFS cloned/shared extents can be double-counted by block sums, sparse/compressed
files differ from logical length, and a live inventory is not snapshot size.
Neither whole disk usage nor advertised runner capacity substitutes for this.

Local native MinIO is **conditional**. The conservative planning budget is two
unique-hardlink logical copies (remote baseline plus one sequential restore),
twice the controlled later changes, plus `max(8 GB, 8192 bytes/source entry)` for
metadata, evidence and working space. This is **not a lower bound, exact deficit,
product workload cap or proof of impossibility**. If free space is below it,
report **local placement not certified**, with measurements, planning shortfall
and compression/shared-allocation uncertainty. Do not claim actual ENOSPC unless
an operation produced it. Do not blindly force the copies onto spare disk.

The current Linux VM's root/virtiofs capacity is not hosted-Mac storage. No
approved private route exists by implication. A changed placement requires an
explicitly approved private route and sufficient measured object storage, plus
native Mac-writable full-restore capacity/metadata support. A new APFS volume in
the same container adds isolation, not space. Do not expose ports, create public
tunnels, buy infrastructure or shrink the source to avoid reporting uncertainty.

Full restores are sequential. Remove only verified task-owned restored copies,
after preserving their complete evidence; do not delete baseline/native remote
objects or ordinary runner files. Default retention is not weakened for space.
The shared 330-minute deadline covers all stages: `deadline.py` supervises the
build/fetch/public-install process group in `build.sh`, and native acceptance
uses the same absolute deadline. A stalled build's entire process group is
terminated and its owned leader reaped, not merely its shell killed. Native finalization independently stops
and bounds/reaps the client, application and services, retains all outcomes,
and aggregates cleanup failures into a nonzero exit. Forced daemon termination
is a cleanup failure, never graceful success. Final acceptance success is emitted
only after cleanup succeeds. Diagnostic/artifact headroom remains within the
six-hour hosted limit; unfinished phases are not successes.

## Manifest and evidence contract

`manifest.py` records every entry in the backup and restored volume trees:
SHA256 of complete regular-file contents; every xattr's length/SHA256 (including
resource forks); native extended ACL text; owner/group, mode, flags, birth/mtime;
symlink targets; and hardlink equivalence classes. Empty directories and special
file identities are not silently dropped. Errors or changes during scanning
fail. Physical inode/device/link counts and access/change times are retained as
observations, not compared as identities after reconstruction. Hardlink topology
is compared. All other recorded metadata is compared strictly. The artificial
outer restore container's metadata is not backed up and is excluded from the
per-volume comparison, not from raw evidence.

Remote backup manifests before and after wipe must agree. Restored manifests
must match **all backed-up volume entries**, including ordinary runner contents.
Known proof files add explicit metadata and resumed-change checks, not coverage
substitution. Unknown backup-root layouts fail instead of guessing which files
are dispensable. The exact current platform layout/metadata fidelity is still a
runtime unknown; do not convert an unknown into a compatibility claim.

Artifacts include native help/command stdout and exit codes, timing JSONL,
source/exclusion/capacity inventories, whole-tree backup/restore manifests and
all differences, native metadata receipts and readback events, recovery PTYs,
MinIO/application/proxy logs, complete object inventories, backup/mount/device
identifiers and Time Machine unified logs. Successful chunk-GET counters are
captured separately around `tmutil restore` itself: earlier manifest reads alone
do not pass the remote-read gate. Proxy JSONL preserves every actual request.
The [fixture README](../test/macos/fixture/README.md) defines exact observations.

On both success and failure, the entrypoint finalizer inventories/hashes every
produced artifact after evidence writers stop, then hands only that task-owned
real-file evidence tree back to the original runner (directories0700/files0600).
It does not make root evidence world-readable or alter system permissions.
Before upload, a separate `always()` workflow step runs as the actual ordinary
uploader identity and reads/hashes every file, checking exact inventory,
ownership and private modes. Missing/unreadable/changed evidence fails the gate;
artifact upload still runs to retain whatever failure evidence is available.

## Portable validation (not Mac proof)

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s test/macos -p 'test_*.py' -v
bash -n test/macos/run.sh test/macos/build.sh
shellcheck test/macos/run.sh test/macos/build.sh
actionlint .github/workflows/macos.yml
# Under the shared heavy-work lock on the development VM:
GOMAXPROCS=2 go test -p 2 -race ./test/macos/fixture
```

Run Python helpers as an ordinary local user with `sudo -n` available: the
ownership regression deliberately creates a task-owned root0600 artifact and
checks its initially denied read, private handoff, then complete read/hash as the
actual uploader. No privileged operations outside test-owned temporary data.
Portable helpers also check full manifest changes/missing/extra data, xattrs/modes,
hardlinks, observational fields, exclusion/error handling, capacity arithmetic,
bounded command evidence and continuous PTY draining/consent. Review regressions
exercise real stalled-build child cleanup and unresponsive daemon reaping,
aggregated service/client failures, success-after-cleanup ordering and ordinary
resumed-entry loss/change against the independent pre-wipe expectation.
Fixture tests use
ordinary local HTTP/SDK fixtures, not actual Apple/Time Machine. First helper run
failed because the *test executable* incorrectly opened `/dev/tty` read/write as
a seekable Python stream; that failure is retained separately from the corrected
passing run. No production application behavior was changed for it.
