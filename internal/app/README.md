# Foreground lifecycle

`main.go` calls `app.Main`. `serve` accepts `-c` and `--log-format text|json`
before or after the command. The logging override is selected before parsing or
loading YAML. Help/version never load configuration or resolve credentials.
Version uses the installed module version when supplied by Go's build information.

Startup holds `state_dir/state.lock` using an exclusive nonblocking OS lock.
This is only a local-authority lock, not a distributed lease. The inode is never
unlinked. Creating the private coordination directory/lock is the only local
state creation before dataset classification and required confirmation.

Every startup lists the remote bucket; failures never imply emptiness. An absent
identity with objects present requires inspection of the newest native backup,
using only an existing unique bootstrap key in encrypted mode. Corrupt selected
points do not trigger an older-point fallback. Existing local metadata must match
the remote native format. New initialization or recovery prompts exclusively on
`/dev/tty`; redirected stdin is not consent. Only a complete `yes` line confirms.
Recovery warns about later-change loss and stopping the old writer on every host.

Writable startup establishes a verified metadata backup before creating a native
session, filesystem or SMB listener. Ordinary restarts may verify/reuse a recent
receipt without moving its original schedule. Recovery always obtains a new
point before writable serving. Read-only startup uses native metadata read-only
mode and a permanently closed maintenance gate; it performs no remote backup or
retention. Read-only recovery imports local SQLite, and read-only initialization
of an empty dataset is rejected. Native lock-only transactions permit byte-range
lock conflicts, unlock and reacquisition on read-only sessions; file and namespace
mutations remain rejected by native read-only enforcement.

Shutdown closes the maintenance gate, cancels scheduled backups, stops accepts,
drains SMB requests and handles, joins any residual native metadata export,
closes the native filesystem/session, metadata engine and transport, and only
then closes the local lock. Native session close joins refresh/stale-session work
and admitted asynchronous compaction/deletion/prefetch; unmounting blocks new
mutable work admission. Any failed close terminates nonzero without advancing
through unsafe cleanup. A 30-second process watchdog terminates a stuck shutdown
while retaining the lock until the OS exits the process. Exit diagnostics have
a 100-millisecond best-effort window so a blocked log pipe cannot prevent exit.
Signals during startup
also have this deadline. Native synchronous recovery has a two-minute operation
budget plus the shutdown deadline; backup operations use two minutes total with
three attempts and explicitly join any timed-out export before releasing state.

Tests in this directory cover argument placement, detached terminal refusal,
confirmation parsing, real OS lock exclusion and hard-exit lifetime, help/version
side-effect avoidance, and JSON parse/config errors in subprocesses. These are
not evidence of a native blocked-I/O shutdown, SMB-to-S3 recovery, power-loss
durability or Time Machine compatibility; the shared Docker suite owns those
integration gates.
