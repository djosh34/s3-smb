# Development

## Layout

- `internal/app`: command line, prompt, state lock, startup, shutdown.
- `internal/config`: YAML loading, validation, secret sources and TLS files.
- `internal/storage`: S3 connection, volume identity, encryption key, JuiceFS setup.
- `internal/backup`: scheduled metadata backups, delete protection, recovery.
- `internal/smb-old/smbfs`: the SMB server's filesystem interface on top of JuiceFS.
- `internal/logging`: `log/slog` setup, secret removal, bridges for JuiceFS logs.
- `internal/juicefs`, `internal/smb-old/smb2`, `internal/thirdparty`: patched upstream
  code, described in [vendored source](vendored.md).
- `test/e2e`: tests that run the built binary against MinIO over SMB.
- `test/macos`: the Time Machine test for GitHub's Mac runners.

## Startup

1. Take an exclusive lock on `state_dir/state.lock`. A second process with the
   same state directory exits.
2. Connect to S3 and list one object. If the listing returns an object, the
   bucket is not empty, even if the listing was truncated. Only a complete,
   empty listing counts as an empty bucket. A failed listing, or a truncated one
   that returned nothing, stops startup.
3. Decide what to do. An empty bucket with no local database asks to initialize.
   A bucket with data and no local database asks to recover from the newest
   metadata backup. Both prompts read `yes` from `/dev/tty`. A local database
   must match the volume identity in the bucket.
4. Recover the chosen backup into a new SQLite file, or open the existing one.
5. Clear file locks left in SQLite by an earlier process.
6. Take a metadata backup, or reuse the last one if it is younger than
   `backup.interval` and its object in S3 still matches. If this fails, SMB does
   not start.
7. Open the JuiceFS filesystem and session, then listen for SMB and start the
   backup schedule.

Read-only mode skips the backups and never deletes data.

## Shutdown

On SIGINT or SIGTERM, or when a scheduled backup fails:

1. Close delete protection and cancel the backup schedule.
2. Close the listener and drain SMB requests.
3. Flush and close every open file handle.
4. Wait for a running metadata backup to finish.
5. Close the JuiceFS filesystem and session, then the metadata engine.
6. Close the S3 connection, then release the state lock.

If any step fails, s3-smb exits with status 1 and keeps the lock until the
process ends. A watchdog ends the process if shutdown takes more than 30 seconds.

## Delete protection

JuiceFS deletes data blocks once deleted files and blocks replaced by compaction
have been in the trash for `backup.trash_days`. Separately, s3-smb removes old
metadata backup objects by the rotation in [recovery](recovery.md); that never
deletes data blocks. Both run only while the newest successful metadata backup
started less than two backup intervals ago. Every delete transaction and every
S3 delete checks this, so a stopped backup schedule stops all deletes.

## Tests

```sh
scripts/check.sh         # PR checks, including fuzz seed replay
scripts/check.sh --gate  # phase and release gates, including fuzz exploration
```

Both modes need Linux ARM64 or AMD64, Bash, curl, tar, Docker, Go 1.26.3,
Python 3 for the MinIO publisher tests, and a C compiler. They run golangci-lint and `go vet` with and without
`-tags smbnext`, and lint the Mac harness with `GOOS=darwin` and `-tags macos`.
They run shellcheck over our shell scripts and actionlint over every workflow,
then check `go mod tidy -diff` and gofmt. `go test -count=1 ./...` runs the Go
unit tests, including the untagged helpers in `test/macos/helpers`. It does not
run Time Machine. The Docker step runs `go test -race -shuffle=on` over the
untagged packages with MinIO available, including the Linux integration tests.
It also runs `go test -race -shuffle=on -count=1 -tags smbnext ./internal/app/...`
to check the new server's startup, shutdown and app wiring.
The gofmt check skips vendored code (`internal/juicefs`, `internal/thirdparty`)
and the frozen SMB server (`internal/smb-old`).
`scripts/lint-tools.sh` downloads golangci-lint 2.14.0, shellcheck 0.11.0 and
actionlint 1.7.12 from their release archives and checks their pinned SHA-256
hashes. The binaries live under `${XDG_CACHE_HOME:-$HOME/.cache}/s3-smb-lint`,
keyed by the installer contents and CPU architecture. Both local checks and CI
call these binaries by full path, not tools on PATH. actionlint uses the same
pinned shellcheck for inline shell. It does not use an optional pyflakes on PATH.
To update a tool, change its version and both archive hashes in the installer.

`.golangci.yml` enables the strict Go linters and the gofumpt and goimports
formatters. It always excludes `internal/juicefs`, `internal/thirdparty` and
`internal/smb-old`. An explicit list excludes today's other Go packages and
root files until P5. New packages, including new packages under `test/`, get
all checks. Exclusions match files in existing packages, not new subpackages.
Lint still loads dependencies; findings from excluded paths are not reported.

Fix lint findings rather than suppressing them. If a suppression is needed,
use `//nolint:<linter> // <reason>`. nolintlint requires the name and reason.
Reviewers check each suppression. Panic, recover and fatal logging are banned;
`fmt.Print*` is allowed in tests. `os.Exit` is allowed only in the root
`main.go` or a command entry point at `cmd/<command>/main.go`.

The lint tests create temporary modules to check the exclusions, new-package
findings, both build selections, formatting and suppression syntax. Installer
tests use mock downloads to check both CPU architectures, cache reuse and errors.

PR mode uses ordinary `go test` to replay fuzz seeds and saved inputs in
`testdata/fuzz`. Gate mode also discovers every fuzz target and explores each
for one minute with two workers. Other checks use Go's CPU defaults, including
inside Docker. The entry point leaves `GOMAXPROCS` and `GOFLAGS` unchanged.
Tests receive `S3_SMB_CHECK_MODE=pr` or `gate`, including inside Docker, so they
can choose short or full-length outage tests.
The namespace measurement test runs an encrypted snapshot and cold recovery
through the daemon and MinIO. PR mode seeds 4,096 bands; gate mode seeds 524,288.
It checks peak daemon RSS and elapsed time and writes `namespace-measurement.json`
to the log directory. CI keeps that file on passing runs too. See the measured
costs and limits in [recovery](recovery.md#measured-snapshot-costs-and-limits).

Go saves failing fuzz inputs in the package's `testdata/fuzz` directory. Keep
those inputs as regression tests.

The Docker step runs every package with MinIO available, including `test/e2e`
and the storage integration tests. It builds the application without the race
detector and runs the tests with `-race -shuffle=on`. The source is mounted
read-only. Each run has its own containers and network, with no lock. MinIO
data goes away with the containers.

`test/Dockerfile` holds the MinIO release and source commit as ARGs, and copies
MinIO from `ghcr.io/djosh34/minio` by release tag and image digest. It also
pins `samba-testsuite` and `smbclient` to Samba 4.17.12-Debian. The local test image is tagged with
the SHA-256 hash of `test/Dockerfile` and reused while that file is unchanged.
It is not published.

The Mac build and `Publish MinIO` workflow read the same pin from
`test/Dockerfile`. `test/minio/Dockerfile` builds that source commit. The
workflow publishes AMD64 and ARM64 images when the commit changes on `main`,
or when dispatched by hand. It never overwrites an existing release tag.
For a MinIO bump, publish the new release and update the image digest in
`test/Dockerfile` in the same PR that changes the pin.

The tests in `test/e2e` start the built binary, answer its prompt, and read and
write files over signed SMB. They cover authentication, read-only mode, file and
lock operations, missing data, uncompressed objects, startup with a damaged bucket, a
failed scheduled backup, a kill during an S3 upload, and recovery after deleting
all local state, including from a metadata backup taken while files were being
written.

The Docker step also builds a race-enabled `smbnext` daemon and runs
`TestSambaInterop` with `GORACE=halt_on_error=1`. The test checks the daemon's
build information for `-race` and the `smbnext` tag before starting it.
smbclient authenticates and connects to `TimeMachine` with
SMB 3.1.1 and encryption, then quits without listing files. The smbtorture runner
checks tool versions, validates `test/e2e/smbtorture.allowlist` against `--list`,
and runs each exact test ID separately. A failure, skip or missing success fails
the check. M2 has no eligible Samba credit tests; the allowlist records why.
Later milestones add names to that file without changing the runner.

A separate `TestSmbnextE2ESubset` step runs the exact names in
`test/e2e/smbnext.allowlist` against the same race-enabled daemon, also with
`GORACE=halt_on_error=1`. It validates names against Go's test listing and builds
an anchored `-run` expression. Failures, skips (including subtests) and missing
success reports fail the step. An empty list is validated but runs no tests.
File-handler PRs add M3 names as they pass against `smbnext`.

The script prints the directory that holds each daemon's stdout, stderr and
prompt log. Set `S3_SMB_TEST_LOGS` to choose it. Go caches persist in two Docker
volumes: `docker volume rm s3-smb-test-gomod s3-smb-test-gobuild` removes them.

GitHub's `check` job calls `scripts/check.sh` on every pull request and on
`main`. Dispatch the workflow with `gate=true` for a gate run. The job name
`check` is fixed because branch protection requires it. Failed runs upload
daemon logs; test or fuzz failures also upload any `testdata/fuzz` inputs.

## Time Machine end-to-end test

`.github/workflows/macos.yml` runs only when dispatched by hand:

```sh
gh workflow run macos.yml --ref <branch-or-tag> -f mode=acceptance -f server=default
```

Each `macos-15-intel` runner builds s3-smb from the checked-out commit and runs
MinIO locally. `server=default` builds without tags; `server=smbnext` builds with
`-tags smbnext`. The same selection applies to every job in the run.
`test/macos/run.sh` runs `go test -tags macos -count=1 -timeout <limit> -v
./test/macos/...` with sudo for Apple's administrative commands. The Go test
builds the binaries and owns Time Machine state. There are no parallel Time
Machine tests within a job. The test stops itself at least ten minutes before
its Go timeout and has a separate seven-minute cleanup budget. The workflow
leaves another five to ten minutes for uploads before the job timeout.

Evidence includes `mac-harness.log`, numbered command logs, application logs,
`application-revision`, `harness-revision`, `build-tags` and `native-build.txt`
from `go version -m`. Evidence uploads use `always()`, including after timeout
or cancellation. Transfer artifact names use the run ID, not the attempt, so
re-running failed recovery jobs can use a successful earlier producer. The
harness does not install a released version from the Go proxy.

The acceptance backup job first runs the SQLite full-fsync pool test on macOS.
It checks both pragma values on four live connections and four replacements.
The same test runs in the Linux checks. SQLite uses full fsync only on macOS.

One Mac backs up a small test directory with Time Machine,
with most of the disk excluded. A second, fresh Mac gets only the MinIO store,
recovers the dataset, restores the directory with `tmutil restore` and compares
it. Six more Macs each interrupt a second backup. Five of them then restart or
recover s3-smb and restore the first backup. In the machine-loss scenario the
Mac exports the stopped store, and a further fresh Mac recovers it and restores
the first backup. The scenarios kill the application or the Time Machine client.
They do not cut power and do not remove objects from S3. Every interruption,
including machine-loss, requires a nonempty change in remote chunk objects.
The resumed backup must also complete and restore the changed tree. The test
uses a five-minute metadata interval, or one minute for the midpoint scenario;
the product default is one hour.

The scenarios are `server-kill-restart`, `launchd-kill-restart`,
`server-kill-cold`, `server-kill-cold-midpoint`, `client-abort-cold` and
`machine-loss`. `launchd-kill-restart` initializes in the foreground, then
loads [the shipped plist](com.s3-smb.plist) into the system domain with test
paths. It kills s3-smb with SIGKILL while Time Machine is copying and requires
a new launchd PID and a new SMB serving log entry without consent. It captures
changed S3 chunks after the kill and before the new process serves, then
requires the next backup to complete with a matching restore and the same PID.
Cleanup reserves time to unload the job before stopping MinIO, also on failure
or timeout.

To run just one scenario:

```sh
gh workflow run macos.yml --ref <branch-or-tag> -f mode=scenarios -f server=default \
  -f 'scenarios=["server-kill-cold"]'
```

The `discover` mode builds the selected server, lists directories to exclude
and checks the literal exclusion list without running a backup:

```sh
gh workflow run macos.yml --ref <branch-or-tag> -f mode=discover -f server=default
gh workflow run macos.yml --ref <branch-or-tag> -f mode=discover -f server=smbnext
```

## Releasing

1. Make sure the `check` job passes on `main` and run `scripts/check.sh --gate`.
2. Tag the commit as a release candidate, `vX.Y.Z-rc.N`, and push the tag.
3. Run `scripts/check-public-install.sh vX.Y.Z-rc.N` on Linux and on a Mac. It
   installs the version from the Go proxy with empty caches.
4. Run the Time Machine workflow with `--ref vX.Y.Z-rc.N -f server=default`.
   It builds the tagged code; the public proxy install is checked separately.
5. If it passes, tag the same commit `vX.Y.Z`.

Never move or reuse a tag. The Go checksum database keeps the first hash.

## Left out on purpose

- An S3 heartbeat to detect a second writer. It cannot stop a writer that keeps
  running, so the rule stays one writer per dataset.
- A full data scan after recovery. A missing object shows up as a read error.
- A separate key file that you must keep. The key is in the bucket, protected by
  the passphrase.
- SlateDB, Litestream or restic in place of SQLite and JuiceFS metadata backups.
