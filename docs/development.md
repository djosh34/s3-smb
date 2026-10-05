# Development

## Layout

- `internal/app`: command line, prompt, state lock, startup, shutdown.
- `internal/config`: YAML loading, validation, secret sources and TLS files.
- `internal/storage`: S3 connection, volume identity, encryption key, JuiceFS setup.
- `internal/backup`: scheduled metadata backups, delete protection, recovery.
- `internal/logging`: `log/slog` setup, secret removal, bridges for JuiceFS logs.
- `internal/smb`, `internal/smbfs`: the SMB server and its JuiceFS
  filesystem, described in [the SMB server design](smb-design.md).
- `internal/juicefs`, `internal/thirdparty`: patched upstream code, described
  in [vendored source](vendored.md).
- `internal/netfault`, `internal/s3fault`: TCP and S3 fault proxies for tests,
  which cut, slow, stall and fail traffic.
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
5. Take a metadata backup, or reuse the last one if it is younger than
   `backup.interval` and its object in S3 still matches. If this fails, SMB does
   not start.
6. Open the JuiceFS filesystem and session, then listen for SMB and start the
   backup schedule.

Read-only mode skips the backups and never deletes data. The SMB server keeps
byte-range locks in memory, so they end with the process.

## Shutdown

On SIGINT or SIGTERM, or when metadata backup protection expires:

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
S3 delete checks this, so a stopped backup schedule stops all deletes. Failed
metadata backups retry with exponential backoff capped at 30 seconds. They do
not close protection early. A successful retry renews protection; expiry stops
the writer.

## Tests

```sh
scripts/check.sh               # PR checks, including fuzz seed replay
scripts/check.sh --gate        # release gate: full-length outage tests and fuzzing
scripts/check.sh unit          # one part: lint, unit, fuzz or integration
scripts/check.sh --gate fuzz 2/6  # every sixth fuzz target, from the second on
```

Both modes need Linux ARM64 or AMD64, Bash, curl, tar, Docker, Go 1.26.3,
Python 3 for the MinIO publisher tests, and a C compiler. Without a part they
run, in order:

1. `lint`: golangci-lint and `go vet`, also for the Mac test with
   `GOOS=darwin` and `-tags macos`; shellcheck over our shell scripts;
   actionlint over every workflow; `go mod tidy -diff`; gofmt; and the shell
   tests in `test/`.
2. `unit`: `go test -race -shuffle=on -count=1 ./...`, which also replays the
   fuzz seeds and the saved inputs in `testdata/fuzz`. Tests that need MinIO
   skip here.
3. `fuzz`, in gate mode only: every fuzz target runs for one minute with two
   workers.
4. `integration`: the Docker step, described below.

Tests receive `S3_SMB_CHECK_MODE=pr` or `gate`, also inside Docker, so they can
choose short or full-length outage tests. The script leaves `GOMAXPROCS` and
`GOFLAGS` unchanged.

Go saves failing fuzz inputs in the package's `testdata/fuzz` directory. Keep
those inputs as regression tests.

### Lint

`scripts/lint-tools.sh` downloads golangci-lint 2.14.0, shellcheck 0.11.0 and
actionlint 1.7.12 from their release archives and checks their pinned SHA-256
hashes. The binaries live under `${XDG_CACHE_HOME:-$HOME/.cache}/s3-smb-lint`,
keyed by the installer contents and CPU architecture. The checks call them by
full path, not from PATH, and actionlint uses the same shellcheck for inline
shell. To update a tool, change its version and both archive hashes in the
installer.

`.golangci.yml` enables the strict Go linters and the gofumpt and goimports
formatters. It excludes only JuiceFS in `internal/juicefs` and the other
vendored code under `internal/thirdparty`; every other file gets all checks. The
formatters skip the same directories.

Fix lint findings rather than suppressing them. nolintlint requires any
`//nolint` to name the linter and give a reason. Panic, recover and fatal
logging are banned; `fmt.Print*` is allowed in tests. `os.Exit` is allowed only
in the root `main.go`, `cmd/<command>/main.go` and `test/macos/fullsync/main.go`.

`test/lint_config_test.sh` checks the exclusions and the suppression rules in a
throwaway module. `test/lint_tools_test.sh` checks the installer with mock
downloads.

### Docker integration

The Docker step mounts the source read-only and starts MinIO and a test runner
in their own containers and network. It builds the binary with the race
detector, then runs the storage integration tests and `test/e2e` with
`-race -shuffle=on`, MinIO available and `GORACE=halt_on_error=1`.

The tests in `test/e2e` start the built binary, answer its prompt, and read and
write files over signed SMB. They cover authentication, read-only mode, file
operations, missing data, uncompressed objects, startup with a damaged bucket,
S3 outages, a failed scheduled backup, a kill during an S3 upload, and recovery
after deleting all local state, including from a metadata backup taken while
files were being written. A test daemon counts as ready only once its own log
says it serves SMB. If another process takes its port first, the test picks a
new port, at most twice.

`TestNamespaceBackupMeasurements` seeds 4,096 bands in PR mode and 524,288 in
gate mode, then measures the startup metadata backup and a cold recovery. It
checks peak daemon memory and elapsed time and writes
`namespace-measurement.json` to the log directory, which CI keeps on passing
runs too. See the measured costs in
[recovery](recovery.md#measured-snapshot-costs-and-limits).

`TestSambaInterop` refuses a daemon without `-race` in its build information.
Samba's smbclient connects to the share with SMB 3.1.1
and encryption, then each test in `test/e2e/smbtorture.allowlist` runs on its
own, with a five-minute limit, and must report success. Every Samba connection
uses the same client GUID, because the server lets one client in at a time.

The run also gets the chaos tests, `TestChaos*`, which skip without
`S3_SMB_CHAOS_SEED`. They back up through S3 errors, throttling and slow or
cut responses, a 5-minute S3 outage (10 seconds in PR mode), a slow, unsteady
and stalling network, connections cut during reads and writes, durable
reconnects inside and beyond the reconnect window, kills and restarts under
faults with a cold recovery after them, and misbehaving connections next to
the Mac. They check exactly the promises of "What survives which failure",
"S3 outage a backup must survive" and "What reconnect promises". Network
faults go through `internal/netfault` and S3 faults through `internal/s3fault`;
nothing needs privileges. The gate runs more rounds and longer faults.

The Docker tests get 120 minutes in gate mode and 60 minutes in PR mode.

`test/run-linux.sh` picks a random seed and prints it. Every fault, cut point
and file comes from that seed, so a run replays with
`S3_SMB_CHAOS_SEED=<seed> scripts/check.sh`, or in CI with
`gh workflow run check.yml -f chaos_seed=<seed>`. Add `--gate` or
`-f gate=true` to replay a gate run, which draws more rounds. Timing still
differs between runs. Restarts under S3 faults model launchd KeepAlive: startup stops on some
S3 errors, and the test starts the daemon again after a second.

The script prints the directory that holds each daemon's stdout, stderr and
prompt log. Set `S3_SMB_TEST_LOGS` to choose it. Go caches persist in two Docker
volumes: `docker volume rm s3-smb-test-gomod s3-smb-test-gobuild` removes them.

`test/Dockerfile` pins the MinIO release and source commit as ARGs, copies
MinIO from `ghcr.io/djosh34/minio` by release tag and image digest, and pins
`samba-testsuite` and `smbclient` to Samba 4.17.12. The local image is tagged
with the SHA-256 hash of `test/Dockerfile` and reused while that file is
unchanged. It is not published.

The Mac test and the `Publish MinIO` workflow read the same pin.
`test/minio/Dockerfile` builds that source commit. The workflow publishes AMD64
and ARM64 images when the commit changes on `main`, or when dispatched by hand,
and never overwrites an existing release tag. For a MinIO bump, publish the new
release and update the image digest in `test/Dockerfile` in the same PR that
changes the pin.

### CI

The `check` workflow runs on every pull request, on `main` and for merge queue
groups. Each part of `scripts/check.sh` runs as its own job, in parallel: lint,
unit tests and the integration. Dispatch it with `gate=true` for a gate run,
which adds six fuzzing jobs, one per shard, so a gate run takes about as long as
its slowest job. A last job named `check` passes only when every part passed;
the name is fixed because branch protection requires it. A failed integration
uploads daemon logs; test or fuzz failures upload any `testdata/fuzz` inputs.

## Time Machine end-to-end test

`.github/workflows/macos.yml` runs only when dispatched by hand:

```sh
gh workflow run macos.yml --ref <branch-or-tag> -f mode=acceptance
```

Each `macos-15-intel` runner builds s3-smb from the checked-out commit and runs
MinIO locally. [test/macos/README.md](../test/macos/README.md) describes the layout
and the `features` mode. `test/macos/run.sh` runs `go test -tags macos` as
root, which Apple's administrative commands need. The test stops itself at
least ten minutes before its Go timeout and keeps a separate seven-minute
cleanup budget; the workflow leaves time for uploads before the job timeout.

Evidence includes `mac-harness.log`, numbered command logs, application logs,
the application and harness revisions and `go version -m`
output. It is uploaded also after a failure, timeout or cancellation. Transfer
artifact names use the run ID, not the attempt, so a rerun of a failed recovery
job can use an earlier successful backup job.

The backup job first runs the SQLite full-fsync test on macOS, which checks
both pragma values on four live connections and four replacements. The same
test runs in the Linux checks; SQLite uses full fsync only on macOS.

One Mac backs up a small test directory with Time Machine, with most of the
disk excluded. A second, fresh Mac gets only the MinIO store, recovers the
dataset, restores the directory with `tmutil restore` and compares it. Eight
more Macs each interrupt a later backup by killing s3-smb or the Time Machine
client, or by cutting its TCP connection. Five of them then restart or recover
s3-smb and restore the first backup. In `machine-loss` the Mac exports the
stopped store, and a further fresh Mac recovers it and restores the first
backup. No scenario cuts power or removes objects from S3. Every interruption
requires a nonempty change in remote chunk objects, and the resumed backup must
complete and restore the changed tree. The test uses a five-minute metadata
backup interval, or one minute for `server-kill-cold-midpoint`; the product
default is one hour.

The scenarios are `server-kill-restart`, `launchd-kill-restart`,
`server-kill-cold`, `server-kill-cold-midpoint`, `client-abort-cold`,
`machine-loss`, `network-drop` and `network-outage`.

`launchd-kill-restart` initializes in the foreground, then loads
[the shipped plist](com.s3-smb.plist) into the system domain with test paths.
It kills s3-smb with SIGKILL while Time Machine is copying and requires a new
launchd PID and a new SMB serving log entry without consent. It captures
changed S3 chunks after the kill and before the new process serves, then
requires the next backup to complete with a matching restore and the same PID.
Cleanup unloads the job before stopping MinIO, also on failure or timeout.

The network scenarios put `internal/netfault` between every SMB client on the
Mac and the server, including Time Machine and restore mounts, and turn on the
kernel's SMB warning log, restoring the previous level afterwards.
`network-drop` cuts while Time Machine is copying, after at least 128 MiB and
with at least 512 MiB of the four-GiB change left, once S3 chunks have changed.
After five seconds it lets connections through again. The same Time Machine
command must finish, the client log must show one backup start and a successful
reconnect, and the backup must restore the changed tree. A drop measured above
30 seconds fails. If macOS refuses to reconnect because of non-idempotent
requests, the attempt is repeated, up to three times; three refusals end the
job as `not tested` in `network-drop-result.json`, which is not a pass. Other
failures do not retry. `network-outage` holds the drop for at least 45 seconds
and until the client fails on its own, then requires a visible failure, no new
completed backup, the earlier backup restored intact, and a successful next
backup and restore. The server keeps running in both scenarios.

The drop log samples in `test/macos/helpers/testdata/drop` are written from
Apple's SMBClient source, not recorded on a Mac.

```sh
gh workflow run macos.yml --ref <branch-or-tag> -f mode=scenarios \
  -f 'scenarios=["network-drop","network-outage"]'
```

To run just one scenario:

```sh
gh workflow run macos.yml --ref <branch-or-tag> -f mode=scenarios \
  -f 'scenarios=["server-kill-cold"]'
```

The `discover` mode builds s3-smb, lists directories to exclude
and checks the literal exclusion list without running a backup:

```sh
gh workflow run macos.yml --ref <branch-or-tag> -f mode=discover
```

## Releasing

1. Make sure the `check` job passes on `main` and run `scripts/check.sh --gate`.
2. Tag the commit as a release candidate, `vX.Y.Z-rc.N`, and push the tag.
3. Run `scripts/check-public-install.sh vX.Y.Z-rc.N` on Linux and on a Mac. It
   installs the version from the Go proxy with empty caches.
4. Run the Time Machine workflow with `--ref vX.Y.Z-rc.N`.
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
