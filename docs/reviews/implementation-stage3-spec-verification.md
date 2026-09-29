# Stage 3 — Spec fix verification

**Reviewed:** clean immutable `/tmp/s3-smb-acceptance-candidate2`, SHA **19e8ecef4fe0f8320b36d2effe12d8da3881aacc**, delta `git diff b99973b...HEAD`. Original `/tmp/s3-smb-swarm/review-stage3-spec.md` preserved unchanged.

## Dispositions

**P1 resource-fork READ — verified fixed.** `internal/smb2/server/file_tree.go:658–686,748–751` now applies the request offset, bounds returned data by request length, and returns EOF at/beyond the value length without overflowing large offsets. `xattr.go:12–39` loads the bounded native value coherently under the existing mutex; native failures use `statusFromError`, including missing-attribute mapping. Apple's absent/empty information-stream fallback no longer conceals genuine I/O failures. No new store or protocol architecture.

`xattr_read_test.go:13–89` asserts short/offset/tail reads, EOF/maximum offset, zero-length reads, missing/access/I/O statuses, and ranged Apple fallback with genuine-error propagation. Both parent tests passed in candidate2's **unit and race** logs.

Crucially, `test/e2e/operations_test.go` is **unchanged from b99973b**. Its previously failing `TestResourceForkOffsetsAndResize` passed against candidate2's actual executable, signed SMB and MinIO at **2026-09-29 12:45:45 UTC**. It now reaches resize, over-limit errors, complete ordinary-file/resource-fork hashes, full local-state removal and cold-recovery hash verification. Evidence: `release-candidate2/e2e.log`; recovered six-byte fork SHA-256 `c5a178aabff0c18ceb8c7fa35289c24edb65b1d09d8204af8db234c90e06aae9`.

**P2 populated old-cache evidence — verified corrected.** `test/cache/cache_test.go:117–204` preserves the same cache tree across separate processes. A positive-capacity restart succeeds with chunk GETs denied and zero requests, proving retained blocks genuinely work. Zero-capacity restart then attempts remote GETs and fails explicitly, rather than serving cached bytes or empty success. With GETs allowed, another zero-capacity process verifies all 4,194,304 bytes by hash through actual MinIO responses. Both phases require unchanged cache names/content/modes/sizes/mtimes; retained-memory checks remain zero. This closes the specific omission without changing cache policy.

Candidate2's **unit, race and cache phases** passed that test. Earlier `cache-zero-populated{,-race}` runs also exited zero; their recorded cache diffs exactly match candidate2, but their dirty b99973b bases are not mislabeled clean-candidate executions.

**Ledger:** `test/coverage/required.tsv:63–66` correctly narrows the unusable-path description and adds exact mappings for populated-cache nonuse and both new read regressions. The original real E2E requirement remains mandatory.

## Evidence limits

Artifacts above are under `/tmp/s3-smb-swarm/`. These are independently inspected owner-run events and source assertions, not reviewer reruns. No source edits, extra tests or lock contention; no other current-reviewer report consulted. No additional actionable finding in this delta.

At verification, the full frozen `scripts/test-linux.sh release` was **still running**, with no final exit status. Both findings are closed at their verified scope, **not** full Linux acceptance. GitHub Linux CI, public installation and final hosted-Mac full backup/crash/restore remain pending. No project-completion or Time Machine compatibility claim.
