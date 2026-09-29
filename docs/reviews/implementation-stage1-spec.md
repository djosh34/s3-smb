# Stage 1 — Spec

1. **P1 — Required signing remains bypassable.** `internal/smb2/server/conn.go:499–519` exempts client-controlled `MessageId == 0xffffffffffffffff` from both signature verification and required-signature checks. After normal password authentication with required signing, my loopback TCP probe sent an unsigned TREE_CONNECT with that ID and received STATUS_SUCCESS and a tree ID. An on-path attacker can alter requests without knowing the session key. This retained upstream exception leaves the new enforcement incomplete. Contract (#23): **“No silent unsigned success for clients requiring signing.”** Reject reserved client IDs rather than bypass authentication of their contents.

2. **P1 — Enforcing verification breaks signed related compounds.** `internal/smb2/server/conn.go:342–346,500–502` now terminates reception on the verifier's raw SessionId comparison. A related compound member may use the inherited-session sentinel `0xffffffffffffffff`; even with a correct signature it is rejected against the actual session ID. The added verifier probe reproduced `unknown session id returned`. Resolve compound session inheritance for authorization without changing the signed bytes. Contract (#23): **“Preserve signing and Time Machine features”** and **“retain existing session behavior except for required fixes.”** This is a regression from making the upstream verifier error fatal, not an attribution complaint.

3. **P2 — Helper descendants outlive the execution bound.** `internal/config/secret.go:125–149` kills the process group only through `Cmd.Cancel`. When a wrapper exits but its child retains stdout/stderr, WaitDelay closes pipes and returns; the cancellation watcher has already finished, so deferred cancellation never kills the group. A `sh -c` helper spawning `sleep 30` left that child alive after the resolver failed at approximately 202 ms. Cleanup should terminate remaining group members on this return path too. Contract: **“Use bounded helper execution/output.”** No sandbox escape is needed.

No additional concrete packaging/logging behavior finding. Public installation, Docker/MinIO and Mac acceptance remain future gates; these tests do not establish them. The announced post-snapshot comment-attribution correction is not reported as unresolved.

## Reviewed revision and executed evidence

- Checkout: `/tmp/s3-smb-review-stage1`; HEAD `f67262104b174ad448c49ed52813c7ccd516990e`; base `f408f1ac9472d90ffe67a0c3bdd947d01bb18165`.
- Executed `git diff f408f1a...HEAD` and `git log f408f1a..HEAD --oneline`; commits `0856e0b`, `f672621`. Compared native patches against `/tmp/s3-smb-swarm/baseline`. Read contracts and fetched issues 19/20/23/26 using `gh issue view`. Checkout remains clean.
- From that checkout, Go 1.26.3 linux/arm64:
  ```sh
  flock -w 90 /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p 2 ./internal/config ./internal/logging ./internal/packaging ./internal/smb2/server ./internal/juicefs/pkg/utils ./internal/juicefs/pkg/chunk ./internal/thirdparty/xorm/log ./internal/thirdparty/xorm -count=1 -timeout=90s
  ```
  Seven packages passed; chunk failed `TestNativeCapacityDiagnosticsAreDecimal`: its substring check for `32 MB` incorrectly matches the truthful `33.554432 MB`. Log: `/tmp/stage1-spec-tests.log`.
- Additional tests were injected only through an external test overlay, without modifying checkout sources:
  ```sh
  flock -w 90 /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p 2 -overlay=/tmp/stage1-spec-overlay.json ./internal/config ./internal/smb2/server -run '^TestReview' -count=1 -v -timeout=30s
  ```
  All three negative assertions failed, reproducing the findings. Evidence: `/tmp/stage1-spec-probes.log`; test sources `/tmp/stage1-spec-{smb,config}-probe_test.go`. Signing bypass used real loopback SMB with a stub filesystem, not MinIO. Related-compound evidence is an executed verifier probe plus receiver source inspection, not a Mac/client-compatibility test. Helper evidence used a real subprocess; the probe killed the surviving child afterward.
