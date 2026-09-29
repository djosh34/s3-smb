# Native metadata protection

This package uses the pinned JuiceFS `meta.Meta`, native JSON/gzip dump/load,
`object.ObjectStorage` (already prefixed and optionally encrypted), and native
backup rotation. It does not upload the live SQLite file or invent a remote
backup format.

## Integration

1. Hold the local single-authority state lock before creating `Manager` or loading
   metadata. `NewProtection(interval, totalBudget, trashDays)` starts closed.
2. Install `Protection.Check` in `meta.Config.CheckMaintenance` and immediately
   before native object deletion. The bootstrap key remains outside that store's
   data prefix and cleanup. Read-only service keeps deletion disabled.
3. Load/initialize metadata without starting a session. Under the state lock,
   call `meta.ClearOrphanLocks` before session/client access in **both** modes;
   it reclaims only native SID-zero advisory rows left by a crashed read-only
   authority. `CleanupRecoveryStaging` removes only recognized abandoned recovery
   files/directories, leaving unknown contents and symlinks untouched.
   Newly initialized or recovered writable service must call `Manager.Backup`. Ordinary restart may
   call `Reuse`, which checks the UUID and complete decrypted object hash against
   its private local receipt; its original snapshot start/schedule is unchanged.
4. Only then create native sessions and expose SMB. `Run` returning an error is
   fatal to writable serving. `Close` permanently shuts protection. On shutdown
   cancel/join `Run`, call `Manager.Wait`, then close native handles/session/SQL
   and finally release the state lock. A hard process-exit watchdog is required:
   native SQL export/import and filesystem sync are not forcibly interruptible.
   `Recover` is synchronous and needs the same startup watchdog.

The live guard checks wall time, including after suspension and backward-clock
movement. Run rechecks it at least once per second after resume. Protection is
valid only until snapshot start + interval + total retry budget; exhaustion
closes it immediately, not after a multi-day grace period. Writable configuration
requires positive native trash retention strictly longer than that horizon.
Native `meta/base.go` `checkTrash` floors deletion buckets to an hour, but its
production `doCleanupTrash` caller already adds two hours before expiry. That
existing native slack covers rounding; no extra hour margin or grace is added.
Writable trash days are limited to 106751 because native cleanup calculates
`time.Duration(24*days+2)*time.Hour`; the next day value overflows that duration.
This is a native numeric bound, not a workload cap or new retention policy.
Read-only service is exempt because it cannot retire data. `TrashDays=0` cannot
meet writable protection and is rejected, not silently replaced.

## Publication and recovery

Exports use native consistent SQLite snapshot mode at every namespace size,
including trash. Gzip Close, staging file Sync/Close, upload, and full readback
must succeed. Temporary exports are mode 0600 under the application's own staging
directory. Only recognized application staging files are cleaned on restart;
existing directory permissions warn rather than block access.

Each native whole-second backup name is durably reserved with O_EXCL in
`backup-names` before uploading. Reservations deliberately survive failed or
ambiguous uploads and restarts. S3 uses conditional `PutIfAbsent`, including
through native encryption/prefix wrappers. There is no fallback to unconditional
PUT. Existing names, backward clocks, and lost responses cannot replace a prior
point. The two local files/directories (`backup-receipt.json`, `backup-names`) are
success evidence/name bookkeeping, not a second backup format.

List selects no fallback. Inspect verifies the selected native JSON identity,
root, and complete gzip trailer. Recover loads exactly that point into a private
fresh SQLite database, validates it, closes/checkpoints it, and publishes without
replacing an existing active database. Current validated connection credentials,
destination, native key, transport, and retention override export fields. Metadata
validation does **not** verify every file's remote data; real SMB-to-S3 fixture
hashes and explicit missing-data tests belong to integration acceptance.

Native backup rotation remains: all points for two days; daily through two
weeks; weekly through two months; monthly through two years. Native trash/data
retention is separate: retaining an old metadata object does not promise its
referenced blocks still exist. External S3 lifecycle rules can invalidate both
data and backups and must not delete protected objects or the bootstrap key.

## Narrow native corrections and tests

Source base: JuiceFS v1.4.1, commit
`0b90c7db5a929ae6adc5faad948d108efd2c99f9`.

- SQLite FULL is a forced DSN connection setting, including pooled replacements
  and `_sync` aliases. Tests query the effective pragma on simultaneous and new
  connections. A subprocess applies a real OS file-size limit after truncating
  SQLite's WAL: a native SQL statement succeeds but COMMIT fails with
  `SQLITE_IOERR_WRITE`, public native SetXattr returns EIO, and cold reopen
  preserves the earlier value. This is real commit-write error propagation,
  not an isolated fsync-failure or simulated power-loss proof.
- SQLite dump always uses native snapshot mode, serializes the shared snapshot,
  propagates writer errors, and suppresses raw panic payload/stack diagnostics.
- Actual SQL retirement (including native trash batch unlink), compaction,
  delayed-slice cleanup and deletion transactions check protection on every retry and immediately before commit.
  Queued native slice deletion rechecks on dequeue; the object owner additionally
  guards each actual remote delete. Adapter prevents client trash access.
- Session close joins refresh, its cleanup children, asynchronously triggered
  compaction/file deletion and inode prefetch. Harmless process-lifetime loops
  retain their upstream lifecycle.
- Read-only native advisory locks use only a private local lock-row transaction
  exception. Startup orphan SID-zero cleanup requires the already-held exclusive
  authority lock; a real subprocess SIGKILL/reopen test proves reclaim while
  retaining registered-session rows. All namespace/data transactions retain
  EROFS; no global flag toggle.

Run isolated regressions with:

```
flock /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p 2 \
  ./internal/backup ./internal/juicefs/pkg/meta ./internal/juicefs/pkg/vfs
```

Add `-race -count=1` for race tests. These use real native SQLite/file/encryption
plus explicit fault injection, not MinIO. The shared Docker suite must independently
prove actual SMB/S3 recovery and file content. No Mac execution is authorized by
these unit results.
