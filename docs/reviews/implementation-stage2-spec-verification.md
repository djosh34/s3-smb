# Stage 2 spec — targeted fix verification

**Fixed revision:** `b99973b38f16ae23ec3d1618e15f8966d6b97c63`, immutable ordinary clone `/tmp/s3-smb-acceptance-candidate1` (clean worktree).

**Comparison:** `git diff abfb2192185b567346889beb5a1ebf758726c969...HEAD`, limited to my accepted finding and the previously disclosed cross-handle blocker. Original report `/tmp/s3-smb-swarm/review-stage2-spec.md` is unchanged.

## P2: orphan read-only SID-zero locks — resolved by source inspection

`internal/juicefs/pkg/meta/protection.go:18–34` adds a narrow transactional cleanup of **only SID 0** rows in native `flock` and `plock` tables. It uses the existing local-lock transaction exception, without permitting read-only namespace mutation or touching file/object references. Registered sessions retain native cleanup.

`internal/app/serve.go:109,272–298,335` establishes the exclusive state lock, loads/validates metadata, then performs cleanup for **both read-only and writable startup**, before native session/client access. Failure prevents serving. This addresses both persistent conflicts and accidental reuse of an old process's owner number.

Checked-in `internal/juicefs/pkg/meta/orphan_locks_test.go`, `TestReadOnlyOrphanLocksCrashRecovery`, exercises an actual child process holding the OS authority lock plus native flock/range locks, kills it, reacquires the authority lock, and reopens SQLite in each mode. It asserts conflicts exist before cleanup and disappear afterward, preserves a nonzero-session lock and file metadata, and checks read-only namespace creation remains forbidden. This directly covers the reported scenario; application ordering was verified separately in source.

**Requirement:** #22 meaningful native locks/cleanup; implementation contract lines 75 and 100 (locking and crash/restart).

## Known P1: cross-handle Pread freshness — resolved by source inspection

`internal/juicefs/pkg/fs/fs.go:1342–1364` now flushes the native inode-wide writer, propagates errors, refreshes native attributes, and updates an existing reader's length **before** applying EOF/read-length bounds. Native inode/handle identity is retained; synthetic internal files are excluded.

Checked-in `internal/smbfs/coherence_test.go:12–72` covers the original reader-opened-empty/flushed-other-writer scenario, shrink, empty-file EOF, buffered regrowth, and propagation of another handle's failed flush as EIO rather than EOF.

**Requirement:** #22 native positional I/O and boundary fixtures.

## Evidence and limits

No remaining issue identified in these two fixes. This is **source-and-regression inspection**, not an independently executed green test claim. `flock -n /tmp/s3-smb-heavy.lock true` returned 1 (busy); no test/build was launched or queued, avoiding contention with the running local release suite. Owner-reported red/green/race results are not substituted for my execution evidence.

No source changes, broad re-audit, or review of the separately assigned xattr/resize/truncate delta. CI, public installation and Mac acceptance remain pending; this report does not approve those gates or claim the running release suite passed.
