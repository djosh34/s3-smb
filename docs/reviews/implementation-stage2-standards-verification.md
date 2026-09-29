# Stage 2 Standards — focused fix verification

**Fixed revision:** `b99973b38f16ae23ec3d1618e15f8966d6b97c63`, immutable ordinary clone `/tmp/s3-smb-acceptance-candidate1` (clean tracked worktree).

Compared against original reviewed `abfb2192185b567346889beb5a1ebf758726c969` using `git diff abfb219..HEAD`. Original evidence report `/tmp/s3-smb-swarm/review-stage2-standards.md` remains unchanged. This is verification of accepted findings, not a new broad audit.

## Disposition

1. **P2 batch trash retirement — resolved by source inspection.** `internal/juicefs/pkg/meta/sql.go:2834` now runs `doBatchUnlink` through `maintenanceTxn`, whose checks cover each transaction attempt and the final pre-commit boundary (`meta/protection.go:46–55`). `meta/base.go:3213` additionally stops expired cleanup rather than retrying the same directory indefinitely. Checked-in `TestSecurityReviewExpiredTrashBatchRetirement` (`meta/trash_protection_test.go:13–57`) exercises actual `emptyDir → BatchUnlink` against expired protection and asserts preserved inode, namespace edge, symlink target and xattr, zero retirement, and an invoked guard. This directly addresses contract `docs/implementation-plan.md:65` / #24 without replacing native cleanup.

2. **P2 orphan read-only advisory locks — resolved by source inspection.** `meta/protection.go:18–34` adds transactional cleanup restricted explicitly to SID-zero `flock`/`plock` rows. `internal/app/serve.go:296` invokes it in both startup modes, after acquiring the existing state lock (`:109`) and validating metadata, before `NewSession` (`:335`) or listening (`:346`); errors abort startup. `meta/orphan_locks_test.go:19–164` checks actual child SIGKILL, authority-lock exclusion/reacquisition, persisted conflicts before cleanup, successful locks afterward in read-only and writable reopen, preservation of registered-session locks/file metadata, and continued read-only mutation rejection. Native lock conflict machinery remains unchanged. Addresses contract `:75`, #22 and #25.

3. **Retention clarification — verified narrow numeric correction.** `internal/backup/protection.go:23–37` retains strict `interval + budget < days*24h`, bounds addition, and rejects days above 106751 before native duration overflow. Native existing `+2h` remains at `meta/base.go:3289`; no extra `-1h` restriction or grace policy. Boundary/default tests are in `backup/backup_test.go:455–484`; documentation matches in `docs/configuration.md:103–124` and `backup/README.md:30–42`.

## Evidence limits

No remaining issue found in these accepted fixes. Independently inspected source and checked-in regressions; **did not execute tests**, to avoid contention with the active full local release run. Prior green regressions are owner-reported, not newly executed evidence here. Source unchanged; no overlays or subagents created. Additional xattr/Pread deltas are outside this focused verification. This does not claim the running Docker release suite, CI, public installation, or Mac gates complete.
