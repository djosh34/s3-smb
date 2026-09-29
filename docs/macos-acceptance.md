# Hosted-Mac acceptance (#34)

**Acceptance remains pending.** The user-approved replacement in live issues
[#34](https://github.com/djosh34/s3-smb/issues/34) and
[#18](https://github.com/djosh34/s3-smb/issues/18) supersedes older Mac plans.
A build, helper test or successful metadata import is not Time Machine evidence.

## Current execution: labelled positive control

The user authorized continued autonomous execution after the failed empty-password
Time Machine destination attempts; the earlier stop rule is rescinded. Use the
qualified **v0.1.0-rc.6** application for a clearly labelled **synthetic nonempty
password positive control** of normal full backup, fresh-Mac stopped-store recovery
and created-tree restore, then crash/resume after that normal path passes. This
is not a fallback silently satisfying the named-empty Time Machine requirement.

Native named-empty Apple SMB mounting has passed. Empty-password Time Machine
destination setup remains unresolved: tested URL, mounted-path and prompted-empty
routes did not establish a destination. Keep that requirement separately pending
and investigate concrete failures without blocking the labelled control on it.
Record the authentication mode with each result. A positive-control PASS proves
only the executed password-authenticated path; it cannot close the original
named-empty Time Machine requirement or establish unexecuted crash/recovery stages.
Harness-only control changes do not require a new application release or repeated
Linux qualification. No authentication framework or speculative policy is needed.

Observed boundary in [run 36622072611](https://github.com/djosh34/s3-smb/actions/runs/36622072611):
port 1445 allowed destination setup with a nonempty destination ID, but the actual
backup failed when background `backupd` tried to mount it. Native logs report
missing usable persisted credentials (`SecItemCopyMatching -25300`), `OpenSession` error
80 and `BACKUP_FAILED_AUTHENTICATION_ERROR (29)`. Destination setup is not backup
completion. Resolve this concrete credential-persistence boundary through the
native documented route rather than trying blind URL/port/authentication variants.

The full remaining chain is **completed normal full-Mac backup → native metadata
backup and clean stopped-store export → artifact handoff to second fresh Mac →
fresh install/native S3 recovery → Apple restore and created-tree comparison →
crash/resume**. No control or intermediate step is the finish line; issues #18/#34
stay open until all required stages pass. No completed normal backup or later
stage is claimed by the destination-setup result above.

Runs 36623413132 and 36631900733 lost communication with their hosted runner;
no artifact or downloadable log survived. Their boundary/cause remains unproven.
[Run 36640633507](https://github.com/djosh34/s3-smb/actions/runs/36640633507)
subsequently **proved disk exhaustion**: durable progress recorded Data-volume free
space falling from 41.48 GB before backup to zero, and GitHub reported
`System.IO.IOException: No space left on device` writing the runner diagnostic log.
The earlier deaths are consistent with this cause, not independently proven.
This is an operational capacity failure, not evidence of SMB incompatibility.
Keep durable progress and actual free-space observations; neither is an estimator
or a completed-backup claim.

### Authorized CI-only capacity relief

The selected next placement is the **standard `macos-15-intel` runner**, reusing
qualified rc6. Its actual free space is **unknown**, not guaranteed greater than
the previous ARM runner; published standard-runner specifications do not establish
sufficient capacity for this backup.

After building and preserving required binaries/provenance, delete **only exact
disposable build/install/source roots created by this test attempt**. Record those
paths/reasons and `df` before/after. The latest user directive supersedes the
briefly proposed unused-Xcode cleanup, which was not implemented: **do not delete
installed Xcode/SDKs, simulator runtimes, user content or the repository checkout**.
Do not add exclusions to shrink the normal source or restrict it to the created
tree. All installed SDKs remain part of the normal full-Mac source.

Retain durable free-space observations, native Time Machine percent/byte counters
when available, and a couple of bounded task-owned storage footprints. These are
runtime diagnostics—not an eligible-source inventory, size estimator, capacity
gate or guarantee. Do not repeat the ARM setup assuming a few GB of scratch
cleanup is sufficient. Report the Intel run's actual outcome.

## Normal backup and fresh-Mac recovery first

Use two dependent standard GitHub-hosted `macos-15-intel` jobs; each runner is already a Mac VM.
No nested VM, external account, paid infrastructure or public service is needed.
Use native MinIO pinned to the Linux fixture revision
`0d7408fc9969caf07de6a8c3a84f9fbb10a6739e`, with loopback services.

1. On Mac A, freshly install the qualified public application. Create a small
   known tree containing multiple files, nested folders and empty folders. Save
   an independent reference separately from the object store for transfer.
2. Run an actual normal full-Mac Time Machine backup to the application-backed
   SMB share. The original named-empty requirement uses the `timemachine` account
   with an explicit empty password; the separately labelled positive control uses
   a synthetic nonempty password. Keep native exclusions; exclude only recursion-producing
   test infrastructure. Only the attempt-owned scratch cleanup above is permitted
   before backup; preserve installed SDKs, ordinary Apple/build/user content and
   the checkout. Do not restrict backup input to the known tree.
3. Require actual Time Machine completion and a completed remote backup. Wait
   separately for the application's successful native metadata backup containing
   that completed state. Metadata backup and Time Machine backup are different
   events. Keep normal application recovery policy.
4. Cleanly stop clients, application and MinIO before archiving the complete
   MinIO data directory. Upload this stopped-store archive and the independent
   reference as separate GitHub Actions artifacts.
5. On a **second fresh Mac B**, download the artifacts, start the same pinned
   MinIO with that store, freshly install the application and recover through
   its documented S3 recovery inputs and confirmation. Do not transfer Mac A's
   daemon-local database, cache, configuration, receipt or local key files.
6. Select the completed backup in the remote Time Machine image and use Apple's
   actual `tmutil restore` into a fresh output location. Restore and verify only
   the deliberately created tree. Compare paths, entry types and file contents,
   including nested and empty directories, against the independent reference.
   Report missing/extra entries or changed contents. Do not compare tar bytes,
   Apple/system/SDK content, or whole-backup metadata.

The simple path is **create tree → actual full backup → stopped-store artifact
handoff → fresh-Mac recovery/native restore → compare created tree only**.
A generic copy, fixture-only backup, local APFS snapshot restore or manual-user
fallback does not pass.

## Boundaries and operational failures

There is no exhaustive eligible-source traversal, per-entry `tmutil isexcluded`
parser, source-coverage inventory, capacity estimator, size gate or whole-system
manifest comparison. Delete these mechanisms rather than retaining disabled
paths or replacing them with another planner/parser framework. Ordinary disk
space observations and actual native errors remain useful diagnostics.

Storage, artifact size, runtime, permissions and hosted-runner limits are real
operational constraints, not demonstrated feasibility. Report actual failures
(including disk-full and timeout) without shrinking backup input or inventing a
capacity guarantee. Use supported service administration, not privacy/security
bypasses. Do not skip a failed native operation or claim an unexecuted stage.

Do not put real credentials in logs, artifacts or the backed-up source. Disable
checkout credential persistence and keep transient CI authentication material
out of the backup. Use disposable loopback fixture credentials; reconstruct
necessary documented recovery inputs without transferring old machine state.

Preserve concise command/status/error evidence, application and MinIO logs,
completed backup and selected metadata-point identifiers, stopped-store handoff
results, and the small comparison result. Bound cleanup and report failures;
archive only after clean stop. Historical failure evidence stays available,
but publication ceremony must not delay a narrow substantive retry.

## Revisions and iteration

Record **harness commit and application version/revision separately**. Harness
changes may reuse the already-qualified application release; do not publish a
new application version or repeat Linux/release qualification solely for test
scripts or documentation. Run relevant focused harness checks before rerunning.
Product changes require appropriate regression/qualification tests.

The manual entrypoint is `.github/workflows/macos.yml`; dispatch its reviewed
harness ref with the qualified `public_version` input independently.
Portable checks are not Mac acceptance:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s test/macos -p 'test_*.py' -v
bash -n test/macos/run.sh test/macos/build.sh
shellcheck test/macos/run.sh test/macos/build.sh
actionlint .github/workflows/macos.yml
```

## Later milestone: crash and resume

Only after normal fresh-Mac recovery/restore passes, exercise crash/resume using
the same created-tree checks. Protect a completed baseline, modify the test tree,
start a later actual Time Machine backup and observe/hold an actual S3 write
while Time Machine remains active before abruptly killing the application.
An idle kill after a fixed sleep is insufficient. Preserve MinIO and committed
objects; recover through normal policy without handpicking or repairing an older
point. Restore and compare the completed baseline, then resume Time Machine,
complete another backup and natively restore/compare the updated tree. Record
the interruption boundary and selected points. This proves an application kill,
not VM power loss or S3 storage loss.

Issues #18/#34 remain open until the required normal and crash/resume evidence
actually passes.

## History (not current instructions)

Earlier native runs
[36584525709](https://github.com/djosh34/s3-smb/actions/runs/36584525709),
[36590782555](https://github.com/djosh34/s3-smb/actions/runs/36590782555), and
[36599466224](https://github.com/djosh34/s3-smb/actions/runs/36599466224) failed in
superseded harness checks before any Time Machine backup. Their build/install
successes and original failures are preserved in issue #34 and release evidence.
The [help-check disposition](reviews/mac-tmutil-help-disposition.md) and
[original hosted-Mac research](research/github-macos-time-machine.md) are
historical evidence, not requirements to restore the removed machinery.
