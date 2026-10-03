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
go vet ./... && go test ./...        # fast tests, no Docker
scripts/test-linux.sh                # every test, with real SMB and MinIO, in Docker
scripts/test-linux.sh -v ./test/e2e  # arguments go to go test
```

`go test ./...` needs Go 1.26.3 and a C compiler. Tests that need MinIO skip
when `S3_SMB_E2E_ENDPOINT` is unset.

`scripts/test-linux.sh` needs Linux, Docker and Bash. It builds `test/Dockerfile`,
which holds Go 1.26.3 and MinIO built from commit
`0d7408fc9969caf07de6a8c3a84f9fbb10a6739e`. It starts MinIO on a private Docker
network, mounts the source read-only, builds the application and runs
`go test -race` on every package with `S3_SMB_E2E_ENDPOINT` set, so no test
skips. The MinIO data lives in the container and goes away with it.

The tests in `test/e2e` start the built binary, answer its prompt, and read and
write files over signed SMB. They cover authentication, read-only mode, file and
lock operations, missing data, compression, startup with a damaged bucket, a
failed scheduled backup, a kill during an S3 upload, and recovery after deleting
all local state, including from a metadata backup taken while files were being
written.

The script prints the directory that holds each daemon's stdout, stderr and
prompt log. Set `S3_SMB_TEST_LOGS` to choose it. Go caches persist in two Docker
volumes: `docker volume rm s3-smb-test-gomod s3-smb-test-gobuild` removes them.

GitHub runs both commands on every pull request and on `main`.

## Time Machine end-to-end test

`.github/workflows/macos.yml` runs only when dispatched by hand:

```sh
gh workflow run macos.yml -f public_version=vX.Y.Z
```

It installs that version from the Go proxy on `macos-15-intel` runners and runs
MinIO on each Mac. One Mac backs up a small test directory with Time Machine,
with most of the disk excluded. A second, fresh Mac gets only the MinIO store,
recovers the dataset, restores the directory with `tmutil restore` and compares
it. Five more Macs each interrupt a second backup. Four of them then restart or
recover s3-smb and restore the first backup. In the machine-loss scenario the
Mac exports the stopped store, and a further fresh Mac recovers it and restores
the first backup. The scenarios kill the application or the Time Machine client.
They do not cut power and do not remove objects from S3. The `discover` mode
lists the directories to exclude. Last passing run:
https://github.com/djosh34/s3-smb/actions/runs/37098439018

## Releasing

1. Make sure both test workflows pass on `main`.
2. Tag the commit as a release candidate, `vX.Y.Z-rc.N`, and push the tag.
3. Run `scripts/check-public-install.sh vX.Y.Z-rc.N` on Linux and on a Mac. It
   installs the version from the Go proxy with empty caches.
4. Run the Time Machine workflow with `public_version=vX.Y.Z-rc.N`.
5. If it passes, tag the same commit `vX.Y.Z`.

Never move or reuse a tag. The Go checksum database keeps the first hash.

## Left out on purpose

- An S3 heartbeat to detect a second writer. It cannot stop a writer that keeps
  running, so the rule stays one writer per dataset.
- A full data scan after recovery. A missing object shows up as a read error.
- A separate key file that you must keep. The key is in the bucket, protected by
  the passphrase.
- SlateDB, Litestream or restic in place of SQLite and JuiceFS metadata backups.
