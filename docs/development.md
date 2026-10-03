# Development

## Layout

- `internal/app`: command line, prompt, state lock, startup, shutdown.
- `internal/config`: YAML loading, validation, secret sources and TLS files.
- `internal/storage`: S3 connection, volume identity, encryption key, JuiceFS setup.
- `internal/backup`: scheduled metadata backups, delete protection, recovery.
- `internal/smbfs`: the SMB server's filesystem interface on top of JuiceFS.
- `internal/logging`: `log/slog` setup, secret removal, bridges for JuiceFS logs.
- `internal/juicefs`, `internal/smb2`, `internal/thirdparty`: patched upstream
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

Both modes need Linux, Bash, Docker, Go 1.26.3, a C compiler and Python 3.
They check `go mod tidy -diff`, `go vet` with and without `-tags smbnext`, and
gofmt, then run the Mac harness Python unit tests, Go unit tests and
`go test -race -shuffle=on`. Python tests do not write bytecode into the tree.
The gofmt check skips vendored
code (`internal/juicefs`, `internal/thirdparty`) and the frozen SMB server
(`internal/smb2`, `internal/smbfs`, or `internal/smb-old` after the move).
The lint stage in `scripts/check.sh` is where additional linters belong.

PR mode uses ordinary `go test` to replay fuzz seeds and saved inputs in
`testdata/fuzz`. Gate mode also discovers every fuzz target and explores each
for one minute with two workers. Tests receive `S3_SMB_CHECK_MODE=pr` or `gate`,
including inside Docker, so they can choose short or full-length outage tests.
Go saves failing fuzz inputs in the package's `testdata/fuzz` directory. Keep
those inputs as regression tests.

The Docker step runs every package with MinIO available, including `test/e2e`
and the storage integration tests. It builds the application without the race
detector and runs the tests with `-race -shuffle=on`. The source is mounted
read-only. Each run has its own containers and network, with no lock. MinIO
data goes away with the containers.

`test/Dockerfile` copies MinIO from the public image
`ghcr.io/djosh34/minio:RELEASE.2025-04-22T22-12-26Z` and installs
`samba-testsuite` and `smbclient`. The local test image is tagged with the SHA-256
hash of `test/Dockerfile` and reused while that file is unchanged. It is not
published. `test/minio/Dockerfile` builds MinIO from the source commit in
`test/minio/commit`. The `Publish MinIO` workflow publishes AMD64 and ARM64
images when that pin changes on `main`, or when dispatched by hand.

The tests in `test/e2e` start the built binary, answer its prompt, and read and
write files over signed SMB. They cover authentication, read-only mode, file and
lock operations, missing data, compression, startup with a damaged bucket, a
failed scheduled backup, a kill during an S3 upload, and recovery after deleting
all local state, including from a metadata backup taken while files were being
written.

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
`test/macos/run.sh` passes `MAC_SERVER` to `test/macos/build.sh`. The evidence
includes `application-revision`, `harness-revision`, `build-tags` (an empty line
for the default build), `application-build.log` and `native-build.txt` from
`go version -m` on the built binary. The harness does not install a released
version from the Go proxy.

One Mac backs up a small test directory with Time Machine,
with most of the disk excluded. A second, fresh Mac gets only the MinIO store,
recovers the dataset, restores the directory with `tmutil restore` and compares
it. Five more Macs each interrupt a second backup. Four of them then restart or
recover s3-smb and restore the first backup. In the machine-loss scenario the
Mac exports the stopped store, and a further fresh Mac recovers it and restores
the first backup. The scenarios kill the application or the Time Machine client.
They do not cut power and do not remove objects from S3. The `discover` mode
builds the selected server and lists the directories to exclude without running
a backup:

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
