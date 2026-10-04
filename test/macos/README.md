# Mac acceptance

The Go harness runs native commands as root on the disposable Mac runner. The
workflow builds the daemon from the dispatch ref, not a released module.
`MAC_SERVER=smbnext` adds `-tags smbnext` to that build.

## M4 gate

The integration agent starts this run once the new server can serve a backup:

```sh
gh workflow run macos.yml --repo djosh34/s3-smb --ref p3/smb-next -f mode=m4
```

Use `--ref <branch-or-commit>` to test another ref that contains this workflow.
The `m4` mode always selects `smbnext`, regardless of the `server` input. It runs
only the M4 job, not recovery or failed-backup scenarios.

On a fresh mounted share, the harness:

- Writes and reads a user xattr, then compares its value.
- Writes 32 bytes of FinderInfo and compares the hex readback.
- Builds and runs `fullsync`, which writes a file and requires the Darwin
  `fcntl(F_FULLFSYNC)` call to succeed.
- Creates, attaches and detaches an HFS+ sparsebundle on the share. This checks
  interoperability, not sharing conflicts.
- Removes the feature fixtures, then runs the existing baseline Time Machine
  backup flow. That flow requires a completed remote backup from `tmutil`, not
  just a successful `startbackup` exit, and verifies a metadata backup receipt.

Command logs, build tags and the daemon's source revision are uploaded as M4
evidence even if the job fails. Issue #205 stays open until the integration
agent's Mac run passes.

## Linux checks

```sh
go test -race -shuffle=on ./test/macos/helpers ./test/macos/fullsync
GOOS=darwin go vet ./test/macos/...
GOOS=darwin go vet -tags macos ./test/macos/...
```

The first vet command checks the portable helpers and Darwin probe. The second
also compiles the native harness. Linux unit tests use fake native commands;
only a Mac run proves interoperability with the server.
