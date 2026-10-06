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
