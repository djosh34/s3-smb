# Stage 2 — independent spec review

Reviewed immutable `/tmp/s3-smb-review-stage2` at **abfb2192185b567346889beb5a1ebf758726c969**, using `git diff f408f1a...HEAD` and `git log f408f1a..HEAD --oneline` (abfb219, f672621, 0856e0b). Read the contract, CONTEXT, README and issues #21/#22/#24/#25. Compared relevant native patches against `/tmp/s3-smb-swarm/baseline-imports-only`; preserved native implementation is not treated as new code.

## Finding

**P2 — Read-only native locks survive a crashed authority without any reclaimable session.** `internal/juicefs/pkg/meta/sql.go:1238–1245` newly permits persistent lock transactions in read-only mode; `sql_lock.go:165–178` uses `m.sid` for ownership. However, `base.go:777–779` returns from read-only session creation before allocating/recording a session, leaving SID 0. `base.go:986–988` skips its cleanup, and stale-session discovery (`sql.go:3223–3257`) only discovers recorded sessions. Scenario: acquire an exclusive range lock while serving read-only, kill the process, restart using the same SQLite, and open that inode with a different handle number. The dead SID-0 lock still blocks reads/locks indefinitely; restarting writable does not create a discoverable owner for it either. Process-local handle-number reuse can instead alias the dead owner. Add narrow orphan-lock cleanup under the existing state lock and/or correct read-only session ownership; do not replace native locking. **Requirement:** #22 “meaningful byte-range lock conflicts, cancellation and disconnect cleanup”; contract `docs/implementation-plan.md:75,100` (locking/cleanup and crash/restart). **Evidence: source inspection, not executed reproduction.**

## Known snapshot blocker (not a new discovery)

**P1 — Cross-handle read freshness remains unfinished.** `internal/smbfs/fs.go:328` calls native Pread, whose preserved `fs/fs.go:1342–1347` bounds reads by the handle's cached size. Open reader on an empty file, write/flush through another handle: reader returns EOF. Requirement: #22 native positional-I/O/boundary fixtures. The snapshot contains `TestNativeCrossHandleReadCoherence`; this is the explicitly disclosed pending delta, not a claim about live owner progress. **Source inspection only.**

## Execution limits

No source edits, subagents, Mac/CI runs or reviewer findings consulted. External probe: `/tmp/s3-smb-spec-proof/overlay.json`. Attempted `flock -w 90 /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p=2 -overlay=/tmp/s3-smb-spec-proof/overlay.json ./internal/juicefs/pkg/meta -run '^TestSpecReadOnlyLocksSurviveAuthorityRestart$' -count=1 -timeout=30s`; lock acquisition timed out, so the probe did **not** execute. An initial literal `-p2` invocation only reported the root package “[no test files]”; it provides no proof. No broad retry was queued. Docker/MinIO and final Mac gates remain acceptance work, not additional code findings.
