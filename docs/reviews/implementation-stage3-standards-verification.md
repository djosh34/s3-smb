# Stage 3 — Standards fix verification

**Reviewed:** clean immutable `/tmp/s3-smb-acceptance-candidate2`, commit **19e8ecef4fe0f8320b36d2effe12d8da3881aacc**, against `b99973b38f16ae23ec3d1618e15f8966d6b97c63`. This is targeted verification of the accepted resource-fork READ finding, not a new full review of unrelated candidate2 changes. Original `review-stage3-standards.md` is preserved.

## Disposition: P2 verified fixed

- `internal/smb2/server/file_tree.go:658–684,750–752` now forwards the actual READ offset, returns only the requested range, checks EOF before converting/slicing the offset, and handles zero-length requests without slicing. Short requests no longer pass undersized buffers to whole-value native Getxattr.
- `internal/smb2/server/xattr.go:10–39` snapshots the native value under the existing shared xattr mutex, checks the 65,536-byte bound before allocation, and validates the second read's count/errors. Reusing this helper for read/write/resize is a narrow consolidation, not another filesystem or cache.
- `file_tree.go:660–669` retains the absent/empty AFP_AfpInfo default header without concealing genuine native failures. `status.go:18–19` maps a missing native xattr to OBJECT_NAME_NOT_FOUND; I/O and permission failures retain distinct statuses.
- `xattr_read_test.go:13–89` checks short/offset/tail reads, exact/far EOF, zero-length reads, missing/I/O/access errors and ranged Apple defaults with genuine-error propagation. These are encoded-response handler regressions, not independently claimed TCP/MinIO tests. Both parent tests are required by the release ledger.

## Executed evidence inspected

The owner's frozen `scripts/test-linux.sh release` artifacts at `/tmp/s3-smb-swarm/release-candidate2` identify the exact reviewed SHA.

- `unit.log`: both new test parents PASS at **12:42:51 UTC**; all nine resource-fork subcases pass.
- `race.log`: both parents and all nine subcases PASS at **12:43:41 UTC**.
- `e2e.log`: unchanged real signed-SMB/MinIO `TestResourceForkOffsetsAndResize` PASS at **2026-09-29T12:45:45.358909382Z**. It verifies the six-byte stream and unchanged base-file hashes before and after full local-state deletion/recovery, plus resize/offset/over-limit behavior. Candidate1 failed this identical test.
- Git confirms this E2E, its recovery fixture and shared harness/workflow are unchanged from candidate1. Attribution was updated in the SMB patch notes.

No additional actionable standards finding in this fix. Reviewer executed only Git/source/event inspection, with no source edits or extra test workload. At the evidence cutoff, the complete release process still lacked its final exit status; the passing targeted events do not certify the entire release. GitHub Linux CI, public installation and final hosted-Mac full backup/crash/restore remain pending. No project-completion claim.
