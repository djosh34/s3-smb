# S3 Time Machine

A terminal-only Go SMB server that stores files in S3-compatible storage through embedded JuiceFS and its supported SQLite metadata backend.

**Status: planning only. Nothing has been implemented or validated. Do not use this project for real backups yet.**

## First-release direction

- Install with `go install`; no GUI.
- Portable bundled C/CGo code is allowed. Installation may need a normal C compiler and platform SDK, but no separately installed third-party native libraries.
- Focused `macos-fuse-t/go-smb2` fork and direct in-process JuiceFS adapter; no FUSE dependency or mount.
- Reuse JuiceFS's native caching. Explicitly support remote datasets larger than the daemon's available local storage.
- Default to `127.0.0.1` with simple authentication. Allow explicit `0.0.0.0` binding.
- Linux/ARM64 development and testing first. Preserve go-smb2's Time Machine-related SMB features. The user will test with a Mac later.
- Access files through SMB. No custom browser or migration tool.
- Use JuiceFS encryption. Back up metadata to S3 hourly and retain deleted/replaced data through JuiceFS trash for 14 days by default. Both settings are configurable.
- Recover to the same S3 dataset and resume normal writes through JuiceFS's supported restore path. Also support an optional read-only mode. Verify the recovery path before claiming compatibility.
- Recover on a fresh installation using S3 and externally saved secrets, without any files from the old machine. Losing changes since the last successful metadata backup is acceptable. Prefer JuiceFS's native periodic backups over per-write remote metadata synchronization.
- No separate backup-management product, Time Machine scheduler, or custom backup-history browser.

## Planning

The [planning map](https://github.com/djosh34/s3-time-machine/issues/1) and its child decision issues live on GitHub. Use wayfinder, grilling, domain-modeling, and unslop. Update issue bodies after each round.

The destination is an agreed implementation backlog with parent issues, sub-issues, dependencies, acceptance tests, and correctness/security review requirements. Specify **what to build**, not how autonomous agents coordinate. Avoid speculative infrastructure.

The [handoff](docs/smb-s3-time-machine-handoff.md) preserves research and earlier proposals with the current scope noted at the top. Unconfirmed recommendations are not requirements, and source inspection is not compatibility evidence.

AGPLv3 is the planned license direction; the exact grant and dependency notices remain to be finalized before implementation/distribution.
