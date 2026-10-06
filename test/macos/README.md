# Mac acceptance

`.github/workflows/macos.yml` runs the Time Machine test on GitHub's Mac
runners. Each job checks out the dispatch ref and calls `run.sh`, which runs
`go test -tags macos ./test/macos/...` as root. The test builds s3-smb from the
checkout and MinIO from the commit pinned in `test/Dockerfile`, then drives
Time Machine with Apple's own commands.

- `*_test.go` with the `macos` tag: the harness. `TestTimeMachine` runs one
  phase, set by `MAC_PHASE`: `discover`, `backup`, `recover` or `scenario`.
- `helpers`: parsers and pass or fail checks that do not need a Mac. They have
  Linux unit tests.

Scenarios beyond the failed-backup ones:

- `incrementals`: a first backup, three incrementals and two restores.
- `s3-outage`: S3 is cut for 5 minutes in the middle of a backup, which must
  go on as the same backup, with no request that macOS timed out.
- `s3-outage-long`: S3 is cut for 6.5 minutes, longer than s3-smb waits on
  it. The backup may fail if Time Machine shows it; a backup it reports as
  good must restore exactly. The macOS log of failed writes is kept.
- `thinning`: `tmutil delete` removes the only backup of a file, whose chunks
  must reach the trash and then be deleted.
- `rollback`: s3-smb is killed right after a copy lands in the middle of a
  backup and loses its data folder. The next server restores that copy.
  Backup A must restore and backup C must work.
- `large`: about 7 GiB in 36,000 files and five wide incrementals, the
  medium tree of #598.
- `b2`: a small backup, an incremental and a restore against the B2 test
  bucket. It runs as its own job, `mode: b2`, which empties the bucket after,
  also when the run fails.

`thinning` and `rollback` build s3-smb with the `testcopies` tag, which keeps
2 copies and makes one every 2 minutes. Each run saves the sizes of uploaded,
live and trashed chunks in `storage.json`.

The harness takes a database copy in the bucket as evidence that a backup is
safe in S3. It restarts s3-smb on its data folder to get one, because every
start uploads a copy before it serves. A start with a new data folder after a
kill waits 10 minutes for the killed server's stale lock, so such starts get
15 minutes.

## Linux checks

```sh
go test -race -shuffle=on ./test/macos/...
GOOS=darwin go vet ./test/macos/...
GOOS=darwin go vet -tags macos ./test/macos/...
```

The tests cover `helpers`. The vet commands compile the harness for Darwin.
Only a Mac run shows that the server works with macOS.
