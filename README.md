# S3 Time Machine

A terminal-operated Go daemon exposing an SMB filesystem backed by S3-compatible storage through embedded JuiceFS and its supported SQLite metadata backend.

**Status: planning only. Nothing has been implemented or validated. Do not use this project for real backups yet.**

## First-release direction

- Install with `go install`; no GUI.
- SQLite is the only permitted CGo dependency, including transitive dependencies.
- Focused `macos-fuse-t/go-smb2` fork and direct in-process JuiceFS adapter; no FUSE dependency or mount.
- Reuse JuiceFS's native caching. Explicitly support remote datasets larger than the daemon's available local storage.
- Linux/ARM64 development and testing first. The user will test macOS later; real Time Machine integration is deferred.
- File browsing/restoration through SMB, not a custom browser or whole-machine migration tool.
- Client-controlled encryption and recoverability after losing all local application state remain design requirements. Their precise contracts are still being decided.

## Planning

The canonical [planning map](https://github.com/djosh34/s3-time-machine/issues/1) and its child decision issues live on GitHub. Use wayfinder, grilling, and domain-modeling; update issue bodies after each round.

The destination is an agreed implementation backlog with parent issues, sub-issues, dependencies, acceptance tests, and correctness/security review requirements. Specify **what to build**, not how autonomous agents coordinate. Avoid speculative infrastructure.

The [handoff](docs/smb-s3-time-machine-handoff.md) preserves research and earlier proposals with the current scope noted at the top. Unconfirmed recommendations are not requirements, and source inspection is not compatibility evidence.

AGPLv3 is the planned license direction; the exact grant and dependency notices remain to be finalized before implementation/distribution.
