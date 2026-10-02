# Stage 1 standards — fix verification

**Result: accepted P1 resolved. No concrete remaining issue in this verification scope.**

Reviewed immutable `/tmp/s3-smb-acceptance-candidate1` at **`b99973b38f16ae23ec3d1618e15f8966d6b97c63`**, against original reviewed **`f67262104b174ad448c49ed52813c7ccd516990e`**. `git rev-parse HEAD` matched; `git status --short` was empty. Original report remains unchanged.

## Fix confirmation

- **Reserved client MessageId signing bypass:** `internal/smb2/server/compound.go:13–15` now rejects `0xffffffffffffffff` in every request, before dispatch. `conn.go:281–309` applies this validation and terminates on verification failure; `conn.go:448–470` removes the former MessageId exemption from signature enforcement. Related-compound inherited **SessionId** handling does not reinstate a MessageId exemption.
- **Narrow unauthenticated-session guard:** `server.go:399–406` rejects TREE_CONNECT and other protected operations without an active authenticated request session, returning `STATUS_USER_SESSION_DELETED`. Completed session state is published before sending authentication success (`server.go:1133–1137`), avoiding a first-request signing-gate race.
- Checked-in regressions cover unsigned/tampered reserved-ID requests and reject a successful TREE_CONNECT **wire response**, rather than relying solely on client API failure (`auth_wire_test.go:169–200`). `session_gate_test.go:12–35` checks the unauthenticated wire status.

## Executed evidence inspected

No competing test process launched. Inspected the coordinator’s ongoing local Docker run:

```sh
S3_SMB_TEST_ARTIFACTS=/tmp/s3-smb-swarm/release-candidate1 scripts/test-linux.sh release
```

`release-candidate1/environment.txt` records the exact fixed revision and clean checkout. `unit.log` and `race.log` contain PASS events for all four signing-rejection subtests, unauthenticated TREE_CONNECT, named-account authentication with and without a password, concurrent authentication, signed related compounds, and invalid inherited-session sentinels. SMB package PASS: **2026-09-29T12:29:25Z** (unit), **12:30:03Z** (race).

Runner commands (`GOMAXPROCS=2`): `go test -p 2 -count=1 -timeout=15m -json "${packages[@]}"` and `go test -race -p 2 -count=1 -timeout=20m -json "${packages[@]}"`; package selection is recorded in `packages.txt` (excluding `/test/e2e`). Both phases marked PASS in `phases.tsv`.

These regressions exercise native SMB over TCP with a fixture filesystem, not full application/MinIO acceptance. The whole release run was still in progress when inspected. No CI/public-install/Mac completion claimed. Additional xattr corrections and unrelated deltas were outside this focused verification; no source changes or smell refactoring performed.
