# Stage 3 — Standards

## Documented breach

**P2 — Resource-fork READ ignores offset and cannot reliably return EOF.** `internal/smb2/server/file_tree.go:658–690,759–760` sends the requested-size buffer directly to whole-value `Getxattr`, without applying `ReadRequest.Offset()`. Repeated reads therefore return the same prefix; a short request instead produces ERANGE, translated to ACCESS_DENIED. This breaks the newly integrated resource-fork write/resize path, not merely a stylistic upstream convention.

**Actual reproduction:** the owner's exact candidate `scripts/test-linux.sh release` failed `TestResourceForkOffsetsAndResize` at `test/e2e/operations_test.go:115`. After writing `abcdef` and overwriting offset 2 with `XY`, signed SMB `ReadFile` returned repeated `abXYef` rather than six bytes, then “permission denied.” Evidence: `/tmp/s3-smb-swarm/release-candidate1/e2e.log`, failure event `2026-09-29T12:31:42.116728398Z`. No remote data loss is demonstrated.

**Requirement:** `docs/implementation-plan.md:75,96` preserves Time Machine-related protocol behavior and requires filesystem/error integration tests; issue #27 requires meaningful real SMB assertions.

**Correction:** narrowly implement bounded resource-fork reads respecting offset, requested length and EOF, with native errors propagated; preserve the special AFP_AfpInfo behavior. Add short-buffer/nonzero-offset/exact-EOF wire regressions and retain this full MinIO/cold-recovery test. Re-run the full release suite on the corrected frozen revision before CI.

## Other standards conclusions

No additional actionable documented breaches or judgement-call smells found in the scoped review. Native conventions are not rewrite targets. The workflow calls the sole Docker entrypoint with the same pinned fixture; the ledger correctly rejects this failure, rather than converting missing coverage into release success. Source/install/recovery documentation distinguishes Linux evidence, public installation and final Mac acceptance. Stage-2 trash-retirement/orphan-lock corrections remain narrow and attributed.

## Revision and executed evidence

- Clean immutable clone `/tmp/s3-smb-acceptance-candidate1`, **b99973b38f16ae23ec3d1618e15f8966d6b97c63**. Compared final delta `abfb219...HEAD` and scoped overall changes `f408f1a...HEAD`; log: `b99973b`, `abfb219`, `f672621`, `0856e0b`.
- Read requested contract/domain/user docs, issues #27/#28, published earlier standards reports/dispositions, actual entrypoint/fixture/tests and native corrections. Verified all **359** relocated baseline hashes against the manifest; this is baseline consistency, not independent upstream retrieval.
- Owner run finished during review: `release-candidate1/exit-status` **1**; unit, race, transport, cache, credentials, packaging and build PASS; E2E and coverage FAIL. Ledger: **1/104 unproven**, the resource-fork case. Unit/race events confirm trash-retirement, orphan-lock crash/reopen and cross-handle coherence regressions pass. Failure-artifact preflight passed its negative assertions.
- Reviewer ran source/Git/log/hash inspection only; no extra Go/Docker tests, source edits, subagents, CI/Mac execution or other current reviewer report.

**Disposition: Linux candidate blocked.** Public Linux install, GitHub Linux CI and final hosted-Mac full backup/crash/restore remain pending; project completion is not claimed.
