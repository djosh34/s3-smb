# S3 Time Machine

A planned macOS application that lets native Time Machine back up through a local SMB server to S3-compatible storage, using embedded JuiceFS and its supported SQLite metadata backend.

**Status: planning only. Nothing has been implemented or validated. Do not use this project for real backups yet.**

## Direction

- Go, with CGo accepted for SQLite.
- Local SMB with a focused `macos-fuse-t/go-smb2` fork and direct JuiceFS adapter; no FUSE mount or separate server.
- A bounded local cache rather than a complete local backup copy.
- Client-controlled encryption and recovery after losing the Mac and all local application state.
- A failed new backup must not destroy a previously protected recovery point.

## Planning

The canonical planning map and decision tickets live in [GitHub Issues](https://github.com/djosh34/s3-time-machine/issues). Planning uses wayfinder, grilling, and domain-modeling. Decisions must be confirmed rather than inferred from recommendations.

The destination is an agreed implementation backlog with parent issues, sub-issues, dependencies, acceptance tests, and correctness/security review requirements. It specifies **what to build**, not how autonomous agents coordinate. Keep scope pragmatic; do not add speculative infrastructure.

The [original handoff](docs/smb-s3-time-machine-handoff.md) records prior requirements, proposals, and source-inspection findings. It is historical input, not proof that the integration works or that every proposed policy was accepted.

AGPLv3 is the planned license direction; the exact grant and dependency notices remain to be finalized before implementation/distribution.
