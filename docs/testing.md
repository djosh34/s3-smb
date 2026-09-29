# Linux tests

## One local/CI entrypoint

Requires Linux, Docker with buildx, Bash and `flock`. No production S3 account,
privileged container, host SMB port, or FUSE mount is used.

```sh
scripts/test-linux.sh                    # implemented unit + race + transport + SMB E2E
scripts/test-linux.sh unit ./internal/config
scripts/test-linux.sh race ./internal/smbfs
scripts/test-linux.sh transport ./internal/storage
scripts/test-linux.sh e2e ./test/e2e
scripts/test-linux.sh release            # same suite PLUS fail-closed completeness ledger
```

`test/coverage/required.tsv` distinguishes implemented assertions from unimplemented
contract cases. The release gate requires exact passing test events: skipped,
missing and `UNIMPLEMENTED` cases fail. Required parent tests also fail coverage
when a child was skipped. A later dedicated execution can replace an earlier
integration skip. Go produces the suite events; the shell artifact-retention
preflight records its event only after its actual assertions pass. A successful development command does **not**
mean the release is complete. Native fault tests are included, but are not represented
as network fault evidence. No Linux test proves Time Machine compatibility.

The runner and MinIO use the same `test/Dockerfile`: Go **1.26.3-bookworm**, pinned
multiarchitecture image digest, and MinIO **RELEASE.2025-04-22T22-12-26Z**, compiled
from commit `0d7408fc9969caf07de6a8c3a84f9fbb10a6739e`. Published MinIO image pulls
were unavailable when the fixture was established, so source compilation is used
rather than an unpinned alternative. The future Mac fixture must use this revision.
No Mac job has been run by this test entrypoint.

MinIO runs on a private per-run Docker network with disposable synthetic credentials
and a new labeled data volume. The runner sees `http://minio:9000`; transport tests
put a local HTTPS/mTLS proxy before that actual MinIO service. Explicit virtual-host
and path-style assertions are the same locally and in CI. Source is mounted read-only;
no host HOME/AWS credentials or application state are mounted.

The disposable runner trusts exactly `/src` as a Git safe directory, because its
root user differs from the mounted checkout owner; VCS stamping remains enabled.

`GOMAXPROCS=2`, `go test -p 2`, and `/tmp/s3-smb-heavy.lock` serialize heavyweight
work on the shared VM. The runner is limited to 5 GB and MinIO to 1 GB. Other local
native builds must acquire the same lock. Do not edit running shell scripts in place:
Bash may continue reading changed byte offsets. Freeze source for final evidence.

## Artifacts and cleanup

By default each run writes `/tmp/s3-smb-test-<timestamp>-<pid>-artifacts`. Override
with `S3_SMB_TEST_ARTIFACTS=/absolute/path`; choose a **fresh** directory for each run.
Artifacts include environment/revision/dirty diff, image identity, build logs, JSON
Go test events, per-phase results, app stdout/stderr and controlling-terminal prompts,
MinIO/container logs, and exit status. App config/state use private disposable runner
paths. Recovery tests remove the entire old local tree, verify every expected file by
SHA-256 over real signed SMB, resume writes, then recover again. Plaintext mode uses
a deliberately nonexistent passphrase command.

On failure, the test-owned MinIO volume remains and its exact removal command is
written to `retained-volume.txt`. Successful runs delete that data volume. Containers
and networks are always removed. Never run a blanket Docker volume prune.

Only content-addressed Go dependency/build caches persist across runs:

```sh
docker volume rm s3-smb-test-go1263-modules s3-smb-test-go1263-build
```

Those are labeled `s3-smb.build-cache=true`; they are **not** application data or
JuiceFS cache volumes. Public install validation separately uses empty caches.

To exercise the harness failure/artifact path intentionally:

```sh
S3_SMB_TEST_INJECT_FAILURE=1 scripts/test-linux.sh unit ./test/coverage
```

This deliberate harness failure is not evidence of an S3 fault or failed daemon.
`release` automatically performs this same negative preflight before taking its
own build lock, checks the nonzero status/logs/container inspection/retained labeled
volume, then removes only that proven test-owned volume. Evidence remains under
`failure-proof/` and `harness.json`.

## CI and release boundaries

`.github/workflows/linux.yml` is manual-dispatch only. Run the full local `release`
command successfully on the exact clean revision **before** dispatching Linux CI.
CI checks the supplied local-tested SHA and invokes the identical entrypoint, images,
fixtures, phases, and ledger; artifacts upload even on failure. ARM64 is used where
available, with actual environment recorded rather than assumed equivalent timing.

`scripts/check-packaging.sh` exercises the local Linux graph/install restrictions;
this is not the public remote install acceptance. Public versioned install, full
contract completion and subsequent real macOS Time Machine backup/recovery remain
separate release gates. Never run Mac before all preceding gates pass.
