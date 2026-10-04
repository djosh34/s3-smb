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

## On-demand network chaos

`network-chaos` is opt-in. It is not in the default scenario list and never runs
on pull requests. Wait until the new server can complete a backup, then ask the
shared integration agent to dispatch it on `p3/smb-next`:

```sh
gh api --method POST repos/djosh34/s3-smb/actions/workflows/macos.yml/dispatches \
  -f ref=p3/smb-next -f inputs[mode]=scenarios -f inputs[server]=smbnext \
  -f 'inputs[scenarios]=["network-chaos"]' -f inputs[chaos_seed]=359
```

Leave `chaos_seed` empty for a random seed. The harness logs the seed and a Mac
replay instruction. `S3_SMB_CHAOS_SEED` passes through the root wrapper; it uses
an unsigned decimal integer. Replay preserves the fault plan, not Time Machine's
traffic timing or the random file contents.

The scenario makes a baseline backup, adds a four-GiB file, and waits for the
existing band-write gate and new remote chunks. It then applies eight seconds
of seeded latency and jitter through the Go proxy, followed by a seeded stall
of five to ten seconds. Delay applies per forwarded buffer, not per packet.
There are no connection cuts, drops or packet-filter commands. The measured
stall must be shorter than 30 seconds even if the runner is delayed.

The same backup must finish. Its bounded Mac log window must show exactly one
backup start and no non-idempotent reconnect refusal. A reconnect message is
not required for latency or stalls. The completed native backup list must
contain the new backup, whose restored files must match the updated manifest.
The baseline backup is restored and checked too. Missing evidence fails the
run; this scenario has no retry or not-tested pass.

Artifacts include the seed, fault plan, actual phase times, band-write status,
Mac logs, daemon logs and both restore manifests. After shutdown, all daemon
generation logs are checked for panics, fatal errors and race reports. Linux
helper tests and Darwin vet are development checks, not Mac acceptance proof.
The PR stays draft until its shared dependencies and required server handlers
land and the shared integration run can provide real backup evidence.

## Linux checks

```sh
go test -race -shuffle=on ./test/macos/helpers ./test/macos/fullsync
GOOS=darwin go vet ./test/macos/...
GOOS=darwin go vet -tags macos ./test/macos/...
```

The first vet command checks the portable helpers and Darwin probe. The second
also compiles the native harness. Linux unit tests use fake native commands;
only a Mac run proves interoperability with the server.
