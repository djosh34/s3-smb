# Mac acceptance

`.github/workflows/macos.yml` runs the Time Machine test on GitHub's Mac
runners. Each job checks out the dispatch ref and calls `run.sh`, which runs
`go test -tags macos ./test/macos/...` as root. The test builds s3-smb from the
checkout and MinIO from the commit pinned in `test/Dockerfile`, then drives
Time Machine with Apple's own commands.

- `*_test.go` with the `macos` tag: the harness. `TestTimeMachine` runs one
  phase, set by `MAC_PHASE`: `discover`, `backup`, `features`, `recover` or
  `scenario`.
- `helpers`: parsers and pass or fail checks that do not need a Mac. They have
  Linux unit tests.
- `fullsync`: a probe that requires `fcntl(F_FULLFSYNC)` to succeed on a file
  on the share. It builds on every platform and fails outside Darwin.

## Share features

```sh
gh workflow run macos.yml --ref <branch-or-commit> -f mode=features
```

On a fresh mounted share it:

- Writes and reads back a user xattr.
- Writes 32 bytes of FinderInfo and compares the readback.
- Runs `fullsync`.
- Creates an HFS+ sparsebundle on the share, attaches it and detaches it.
- Removes these files, then runs the baseline Time Machine backup. It requires
  a completed backup in `tmutil`'s list, not just a successful `startbackup`
  exit, and a metadata backup receipt in S3.

Command logs and the source revision are uploaded as evidence, also
when the job fails.

## Linux checks

```sh
go test -race -shuffle=on ./test/macos/...
GOOS=darwin go vet ./test/macos/...
GOOS=darwin go vet -tags macos ./test/macos/...
```

The tests cover `helpers` and the `fullsync` failure outside Darwin. The vet
commands compile `fullsync` and the harness for Darwin. Only a Mac run shows
that the server works with macOS.
