# Stage 2 — standards

Reviewed immutable `/tmp/s3-smb-review-stage2` at **abfb2192185b567346889beb5a1ebf758726c969**, since `f408f1ac9472d90ffe67a0c3bdd947d01bb18165`. Used `git diff f408f1a...HEAD`; commits: `abfb219`, `f672621`, `0856e0b`. Read contract, CONTEXT, README, package ownership documentation and issues #21/#22/#24/#25. Native patches compared with `/tmp/s3-smb-swarm/baseline-imports-only`; preserved implementations are not treated as new code.

## Documented boundary/lifetime breaches

- **P2 — protection integration misses native batch trash retirement.** `internal/juicefs/pkg/meta/sql.go:2834` still uses ordinary `m.txn` in `doBatchUnlink`, unlike the newly guarded single-unlink path. Actual `cleanupTrash → CleanupTrashBefore → emptyDir → BatchUnlink` reaches this transaction. When a queued cleanup resumes after protection expires, it can remove trash entries, inode attributes and xattrs without consulting `CheckMaintenance`. The later chunk/object guards do not undo that metadata retirement. This is an omission in the new protection integration, not a request to rewrite preserved native cleanup. Route this existing batch transaction through `maintenanceTxn` and test the real trash path after expiry. **Requirement:** contract `docs/implementation-plan.md:65`, “Guard actual native reference retirement/deletion, including queued work”; #24 protection/retirement ordering. **Evidence: source inspection; no demonstrated remote data loss.**

- **P2 — newly enabled read-only lock writes lack a crash-cleanup owner.** `internal/juicefs/pkg/meta/sql.go:1238–1245` / `sql_lock.go:170` permit persistent lock transactions in read-only mode, but native `base.go:777–779` returns before allocating/registering a session; `base.go:986–988` also skips cleanup for SID 0. A read-only process killed while holding locks leaves SID-0 rows; restart/stale-session cleanup cannot identify their dead owner. Different subsequent handle IDs encounter permanent conflicts (and reused IDs can inherit the old identity). Pair this narrow read-only exception with authority-scoped lock lifetime/cleanup under the existing state lock, preserving native conflict checking. **Requirement:** contract `:75` native locking and handle/connection cleanup; #22 meaningful conflicts/cleanup; #25 lifecycle ownership. **Evidence: source inspection, not an executed crash test.**

No standalone heuristic/style findings or broad rewrite recommendations. Known pending cross-handle Pread correction is excluded. Docker/MinIO and release acceptance remain unproven here.

## Execution limits

Source unchanged. External probe overlay: `/tmp/s3-smb-swarm/standards-stage2-overlay.json`. Corrected targeted command:

`flock -w 90 /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p 2 -overlay /tmp/s3-smb-swarm/standards-stage2-overlay.json ./internal/juicefs/pkg/meta -run '^TestStage2' -count=1 -timeout 30s`

Lock acquisition timed out (exit 1, empty output); probes were **not executed**. Earlier literal `-p2` invocation selected the root package and reported no test files—not proof. No further contention, Mac/CI runs, source edits or subagents.
