# Stage 1 — Standards

## Hard documented violations

- **P1 — Required signing still has a reserved-message-ID bypass.** `internal/smb2/server/conn.go:499–521` skips both signature verification and required-signing enforcement when `MessageId == 0xffffffffffffffff`. After normal authentication, an unsigned or tampered request using that ID and the established session/tree IDs passes `tryVerify`; neither `PacketCodec.IsInvalid` nor request dispatch rejects that ID. This defeats the newly added disconnect-on-verification-error path at `conn.go:343–349`. An attacker modifying traffic on a signing-required connection can therefore bypass the intended integrity check. Reject this client-side reserved ID rather than exempting it from verification. **Rule:** issue #23, “No silent unsigned success for clients requiring signing”; `docs/implementation-plan.md:23` requires explicit, tested signing limits. This is a retained upstream exception, not an original-code convention complaint. The finding is source-traced, **not wire-runtime proof**.

## Judgement-call smells

No actionable baseline smells identified. Preserved native conventions, tooling-enforced style, and unfinished application/storage/backup/adapter work were excluded. No additional actionable packaging, configuration, or logging standards violations identified.

## Exact review and evidence

- Reviewed immutable **`f67262104b174ad448c49ed52813c7ccd516990e`**, since **`f408f1ac9472d90ffe67a0c3bdd947d01bb18165`**.
- Executed `git diff f408f1a...HEAD` and `git log f408f1a..HEAD --oneline`: commits `0856e0b`, `f672621`. Read contract/README/CONTEXT, scoped documentation, Xorm CONTRIBUTING, and issues 19/20/23/26 via `gh issue view`.
- Compared scoped native patches with `/tmp/s3-smb-swarm/baseline`; all **359/359** relocated baseline SHA-256 hashes matched the immutable manifest. This is internal baseline consistency, not independent upstream-download verification.
- The source snapshot script, run with `--cache "$(go env GOMODCACHE)"`, failed: cached JuiceFS v1.4.1 LICENSE absent.
- Targeted tests did **not execute**: shared lock unavailable. Latest attempt: `flock -w 60 /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p2 -overlay=/tmp/stage1-standards-overlay.json ./internal/smb2/server -run '^TestReviewSigningReservedMessageID$' -count=1` (exit 1; empty test log). Probe/overlay are outside checkout; source untouched.
- Acknowledge supplied post-snapshot import-only attribution correction and corrected baseline; no duplicate attribution finding. It does not change the reviewed revision.
- Public remote install, Docker matrix, and Mac acceptance remain future gates; none claimed executed.
