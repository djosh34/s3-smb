# Hosted-Mac harness stage 1 dispositions

Review target: `a5bbbe66b5cf9f7c88dc09533dd3d76975184ab4`, baseline
`139b80da357f84c21bb83eb79a863b8545e2c800`. Both independent axes requested
changes. Their original reports, source-level/mock reproductions and immutable
review clone are retained; they are not replaced by this disposition. No Mac
run or production application/module change is involved.

## Four distinct findings — all accepted

| Finding | Narrow correction | Regression evidence |
| --- | --- | --- |
| Standards P1 / Spec P1: required root0600 evidence unreadable by ordinary uploader | `artifacts.py` inventories/hashes only the task-owned real-file evidence tree and transfers private0700/0600 ownership to the original runner. The entrypoint EXIT finalizer runs on success/failure. A separate workflow gate verifies the exact complete inventory, ownership/modes and every file hash as the actual uploader identity before upload. | Real local sudo-created root0600 evidence is initially unreadable by ordinary user; handoff permits complete read/hash while retaining0600. Modified evidence fails verification. Actual Darwin-only entrypoint failure on Linux still produces a complete verified private handoff. No Mac uploader result claimed. |
| Standards P2 / Spec P2: cleanup can suppress failure, discard service outcomes or leave client uncollected | Finalization attempts each owned cleanup independently, bounds/reaps clients/services, closes their logs, records statuses and aggregates errors. Daemon timeout escalates to task-owned SIGKILL/reap but still fails. `execute()` emits success only after both scenario and finalization succeed. | Portable reviewer-pattern regressions require failure for daemon/service exit errors and stopbackup timeout while still reaping other processes/closing logs. Real ignore-TERM child is forcibly reaped and reported failed. Ordering test rejects success after failed cleanup. |
| Standards P2: shared deadline did not supervise build/fetch/public installation | Move the unchanged native build sequence into `build.sh`; `deadline.py` supervises that complete process group against the same absolute epoch used by native acceptance. TERM/KILL covers compiler/download descendants and the leader is reaped. Native cleanup remains separately bounded before artifact handoff. | Real stalled build launches a TERM-ignoring child; the shared deadline returns124 and prevents the child from writing a later marker. Ordinary build exit7 remains7 rather than success. |
| Spec P1: resumed ordinary contents had no independent pre-wipe expectation | Use the same complete pre-wipe capture for both completed baseline and resumed backup. Capture resumed identifier/full manifest before native-point wait/wipe; require that exact identifier and compare its complete recovered manifest to the independent original. | Orchestration-order regression requires resumed capture before wipe and exact identifier/comparison input. Separate ordinary-file mutation/deletion cases leave supplemental proof intact and must fail before native restore. |

## Retained local evidence

Under `/tmp/s3-smb-swarm/`:

- Original reports: `mac-review-stage1-standards.md`, `mac-review-stage1-spec.md`.
- Original immutable review clone: `/tmp/s3-smb-mac-review-a5bbbe6`.
- Original reviewer reproductions/logs: `mac-review-stage1-{standards,spec}-repro.{py,log}`.
- New before-fix regression run: `mac-review-fixes-red.log` — four lifecycle/
  resumed-expectation cases failed against the original implementation.
- First corrected combined helper run: `mac-review-fixes-check1.log` —24 tests
  passed, including actual portable identity/process checks and mocked native
  lifecycle cases. Final immutable-snapshot rerun is recorded in its handoff.
- Negative entrypoint/ownership integration: `mac-entrypoint-failure-handoff.log`;
  exact temporary evidence path in `mac-entrypoint-failure-root`. The entrypoint
  **failed as required** on Linux; its artifact handoff **passed**. This is not
  native Darwin execution or a successful acceptance scenario.
- Earlier test-executable PTY failure remains `mac-helper-tests.log`, distinct
  from its corrected passing results and these later review regressions.

## Still pending

A new frozen snapshot requires narrow independent verification of these fixes.
After clean handoff, execute complete local Linux release and identical Linux CI
at the same final SHA, including the new Go fixture package. Publish a new public
candidate at that exact reviewed/tested SHA and fresh-install it; do not give an
older tag/binary a new source identity. Register the Mac workflow by publishing
the already-tested snapshot, not an untested bootstrap delta. Only then may the
integration owner dispatch the final Mac job.

Execution authorization is not acceptance success. Native CLI/layout, source
permissions/capacity, actual Time Machine behavior and full native metadata/
remote-restore fidelity remain unexecuted. #34/#18 remain open, and prereleases
remain Mac-pending until actual complete evidence passes review.
