# Sleep, lock leases and no-network behaviour

Research for [#635](https://github.com/djosh34/s3-smb/issues/635) (the bucket lock when the Mac sleeps) and [#636](https://github.com/djosh34/s3-smb/issues/636) (how long a request waits for S3). s3-smb code is read at `1518da3`. Every claim is marked:

- **read in source**: I read the code at the linked tag or commit.
- **documented**: an official doc, changelog or man page says so.
- **inferred**: my conclusion from the sources. It was not run on a Mac.

## Summary

1. **restic does not exit on wake. It re-proves ownership first.** It notices a sleep by checking every second whether the wall clock shows 22.5 minutes since the last refresh. Then it freezes all repository I/O except lock files and lists the lock files. It checks that its old lock file still exists, writes a new one, waits 200 ms, checks the old one again and deletes it. If the old file is gone, it cancels the command. The check is sound only because of one restic rule: nobody can take over without first deleting the sleeper's lock file. That takes `restic unlock`, run by a person or a script. Lock files that are merely stale still block everyone else. Borg 2's object-store lock works the same way.
2. **restic's check would prove nothing in s3-smb.** No s3-smb server ever deletes another server's lock key, and takeover happens once a key is stale. So "my key is still there" is always true. A clean exit by the server that took over deletes that server's own key, so the lock folder can look untouched afterwards.
3. **s3-smb has a lasting trace of a takeover: the start copy.** Every start uploads `db/<highest seen + 2>-...`, which is above anything the sleeping run has captured. The new server deletes nothing until that copy has landed, and cleanup never removes the newest copies. A woken run can therefore prove that nobody took over in three steps, before any other write: PUT its own lock key, LIST `lock/`, LIST `db/`. Putting the key before listing closes the race with a server that is starting at the same moment. Holes and conditions are listed in section 4. The biggest one: a retry that the SDK already started before the sleep keeps going after wake without passing the lease check.
4. **Go on macOS:** the monotonic clock (`mach_absolute_time`) stops during sleep. Timers and `time.Sleep` fire late by about the length of the sleep. Go does not fix this, and the upstream proposal is still open. `CLOCK_MONOTONIC` on macOS counts sleep and never jumps, and `golang.org/x/sys/unix.ClockGettime` can read it.
5. **No network:** a refused or unroutable connection is retried with backoff (up to 2, 4, 8 and 16 s, then 20 s flat) until the 6-minute context ends, about 20 attempts. That is the only stop. A DNS answer that Go reports as "no such host" is **not retried**: the call fails at once. It is not known which of the two macOS returns with no network. This needs a Mac test.
6. **Two more exits that the owner will hit:**
   - With no network and the Mac awake, the lease is not renewed, so s3-smb exits after about 8 minutes. Sleep is not needed for that.
   - The 30-minute copy rule also counts the wall clock, so any sleep over 30 minutes ends the run on wake, even with the lock fixed.
7. **macOS:**
   - The SMB client keeps its session across sleep.
   - Its 2-minute request timeout uses `nanouptime`, which stops during sleep. On wake it does not count the sleep, and a reconnect that a sleep interrupted starts over.
   - Apple documents that Time Machine makes hourly local snapshots, keeps them for 24 hours and uses them when the backup disk is unavailable.
   - I found no primary source for when Time Machine retries a failed network backup. `backupd` is closed source.

## 1. restic and sleep

Versions: restic [v0.19.1](https://github.com/restic/restic/tree/v0.19.1) (`6aa3a51`).

### Where it was added

- [0.15.0 changelog, issue-2715](https://github.com/restic/restic/blob/v0.19.1/changelog/0.15.0_2023-01-12/issue-2715) ([PR 3569](https://github.com/restic/restic/pull/3569)) made the lock strict. A command is cancelled if its lock is not refreshed in time. The changelog's motivating case is a client that enters standby while another host runs `unlock` and `prune`. *Documented.*
- [0.16.0 changelog, issue-4274](https://github.com/restic/restic/blob/v0.19.1/changelog/0.16.0_2023-07-31/issue-4274) ([PR 4374](https://github.com/restic/restic/pull/4374), merged 2023-07-22 as `bdaec8f`) says: "restic first checks whether it is safe to continue the current operation and only throws an error if not". The PR text says the new lock is created "but also checks that the old lock continues to exist until after the new lock was created. This ensures that the lifetimes of both locks overlap". It also says all new backend operations are frozen until the stale lock is refreshed. *Documented.*

### Timings

- The refresh interval is 5 minutes ([repository/lock.go#L29](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L29)). *Read in source.*
- The stale threshold is 30 minutes ([restic/lock.go#L252](https://github.com/restic/restic/blob/v0.19.1/internal/restic/lock.go#L252)). *Read in source.*
- The "refreshability timeout" is 30 min − 1.5 × 5 min = **22.5 minutes** ([repository/lock.go#L35-L37](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L35-L37)). The comment says the margin is for clock drift between clients. *Read in source.*

### How it notices that it slept

- A monitor goroutine polls every 1 s. It compares `time.Now().UnixNano()`, the wall clock, against the wall time of the last successful refresh ([repository/lock.go#L183-L219](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L183-L219)). The comments say why: "time.Now() might use a monotonic timer which is paused during standby" and "timers are paused during standby ... Thus fall back to periodic checks", citing [golang/go#35012](https://github.com/golang/go/issues/35012). *Read in source.*
- So detection uses the wall clock only. A wall clock set backwards hides a sleep. *Inferred.*

### What it checks on wake

All of this is in `tryRefreshStaleLock` and `RefreshStaleLock` ([repository/lock.go#L245-L262](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L245-L262), [restic/lock.go#L332-L396](https://github.com/restic/restic/blob/v0.19.1/internal/restic/lock.go#L332-L396)). *Read in source.*

1. It freezes the backend. `Freeze` "blocks all backend operations except those on lock files" ([backend.go#L126-L131](https://github.com/restic/restic/blob/v0.19.1/internal/backend/backend.go#L126-L131), [sema/backend.go#L40-L61](https://github.com/restic/restic/blob/v0.19.1/internal/backend/sema/backend.go#L40-L61)). The freeze takes a lock that every non-lock operation needs before it starts. An operation already past that point is not stopped. *Inferred from the same lines.*
2. It lists the lock files and checks that its own old lock file still exists. If not, it returns `ErrRemovedLock`.
3. It writes a new lock file and sleeps 200 ms.
4. It lists again and checks that the old lock file still exists. If the list fails or the old file is gone, it deletes the new file and fails.
5. It deletes the old lock file.

**It relies on others deleting stale locks.**
- Lock creation (`checkForOtherLocks`) treats every readable lock of a conflicting kind as a conflict. It does not skip stale ones ([restic/lock.go#L160-L216](https://github.com/restic/restic/blob/v0.19.1/internal/restic/lock.go#L160-L216)). *Read in source.*
- Stale locks are removed only by `restic unlock` (`RemoveStaleLocks`, [repository/lock.go#L273-L294](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L273-L294), [cmd_unlock.go#L18-L20](https://github.com/restic/restic/blob/v0.19.1/cmd/restic/cmd_unlock.go#L18-L20)) or by `unlock --remove-all`. *Read in source.*
- So a second process that takes over must first have deleted the sleeper's file. "Old file still exists after my new file was written" proves nobody took over. Deletion is final, and with list-after-write consistency the two locks overlap in time. *Inferred.*
- [design.rst#L626-L641](https://github.com/restic/restic/blob/v0.19.1/doc/design.rst#L626-L641) says new locks test other locks for staleness. In v0.19.1 code that test only happens in `unlock`. *Read in source.*

**If the check fails:** it cancels the command's context while the backend is still frozen, then logs `Fatal: failed to refresh lock in time` ([repository/lock.go#L253-L258](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L253-L258), [#L239](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L239)). *Read in source.*

**With the backend unreachable after wake:**
- The list calls go through restic's retry backend, which keeps retrying for up to 15 minutes ([global.go#L615](https://github.com/restic/restic/blob/v0.19.1/internal/global/global.go#L615)). It backs off exponentially from 1 s, doubling each time, under the `backend-error-redesign` flag ([backend_retry.go#L108-L128](https://github.com/restic/restic/blob/v0.19.1/internal/backend/retry/backend_retry.go#L108-L128)). That flag is Beta, and Beta flags are on by default ([registry.go#L19](https://github.com/restic/restic/blob/v0.19.1/internal/feature/registry.go#L19), [features.go#L16-L17](https://github.com/restic/restic/blob/v0.19.1/internal/feature/features.go#L16-L17)). *Read in source.*
- So restic stays frozen, writing nothing but lock files, for up to about 15 minutes. Then the check fails and the command ends. *Inferred.*
- The same path runs when the network is simply down for 22.5 minutes with no sleep, so restic also ends a backup after a long outage. *Inferred.*

**Known limits that restic itself documents:**
- `unlock --remove-all` "removes all existing lock files ... make sure that no other restic process is currently using the repository" ([0.15.1 changelog pull-4152](https://github.com/restic/restic/blob/v0.19.1/changelog/0.15.1_2023-01-30/pull-4152), [0.15.0 changelog issue-2715](https://github.com/restic/restic/blob/v0.19.1/changelog/0.15.0_2023-01-12/issue-2715)). *Documented.*
- Clock drift between clients is handled only by the margin of 7.5 minutes ([repository/lock.go#L35-L36](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L35-L36)). *Read in source.*

**A gap I found by reading. Not confirmed upstream.**
- The ordinary refresh goroutine decides whether a refresh is "too old" with `time.Since(lastRefresh)`. `lastRefresh` carries a monotonic reading, so that test leaves out the sleep ([repository/lock.go#L161-L178](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L161-L178)).
- If its 5-minute ticker fires just after wake, before the 1 s monitor does, it runs a normal `Refresh` ([restic/lock.go#L309-L330](https://github.com/restic/restic/blob/v0.19.1/internal/restic/lock.go#L309-L330)). That writes a new file and deletes the old one without the existence check.
- Two outcomes are then possible:
  - The monitor gets the "refreshed" signal and never runs the stale check.
  - The monitor is already blocked sending its force-refresh request while the refresher is blocked sending "refreshed", and neither ever proceeds ([#L174-L177](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L174-L177) vs [#L228-L231](https://github.com/restic/restic/blob/v0.19.1/internal/repository/lock.go#L228-L231)).
- The window is narrow. *Inferred.* Lesson for s3-smb: after a lapse there must be only one path, the check. A plain renewal must not count.

## 2. Other prior art

- **Borg 2 object-store lock** (`storelocking.py` on master [`15738b8`](https://github.com/borgbackup/borg/blob/15738b8fb5f4e3f736b380df4ca3fe79375e08a5/src/borg/storelocking.py), not yet in a 1.x release). This is the closest design to s3-smb's lock.
  - Lock objects are immutable. Acquire is list, create, list again, and the docstring requires list-after-write consistency ([L22-L40](https://github.com/borgbackup/borg/blob/15738b8fb5f4e3f736b380df4ca3fe79375e08a5/src/borg/storelocking.py#L22-L40)). *Documented in source.*
  - Every listing deletes locks it judges stale, naming "suspended laptop" as a cause ([L42-L79](https://github.com/borgbackup/borg/blob/15738b8fb5f4e3f736b380df4ca3fe79375e08a5/src/borg/storelocking.py#L42-L79)). A lock is stale by age only if it looks stale both by the owner's timestamp and by store-side mtime. The store's "now" is extrapolated with `time.monotonic()`, and the comment notes that it does not advance in suspend. *Read in source.*
  - Refresh writes a new object, lists, and gives up with "Our lock was killed by another borg" if the old object is gone ([L651-L692](https://github.com/borgbackup/borg/blob/15738b8fb5f4e3f736b380df4ca3fe79375e08a5/src/borg/storelocking.py#L651-L692)). *Read in source.*
  - The model is the same as restic's: others delete, and the owner detects the deletion.
- **Litestream** [v0.5.17](https://github.com/benbjohnson/litestream/blob/v0.5.17/s3/leaser.go) has an S3 lease file with a 30 s TTL ([L22](https://github.com/benbjohnson/litestream/blob/v0.5.17/s3/leaser.go#L22)).
  - Acquire uses `If-None-Match: *` or `If-Match: <etag>`. Renew uses `If-Match` with the etag of its last write ([L138-L169](https://github.com/benbjohnson/litestream/blob/v0.5.17/s3/leaser.go#L138-L169), [L231-L250](https://github.com/benbjohnson/litestream/blob/v0.5.17/s3/leaser.go#L231-L250)). *Read in source.*
  - After a sleep, a renewal fails if anyone rewrote the lease, because the storage checks the etag. This is the conditional-write route that s3-smb ruled out for Garage ([#584](https://github.com/djosh34/s3-smb/issues/584)). *Inferred.*
  - Expiry is judged by the client's wall clock ([leaser.go#L38-L40](https://github.com/benbjohnson/litestream/blob/v0.5.17/leaser.go#L38-L40)). *Read in source.*
- **LiteFS** [v0.5.14](https://github.com/superfly/litefs/blob/v0.5.14/consul/consul.go) leases through a Consul session: TTL 10 s, lock delay 1 s ([L21-L22](https://github.com/superfly/litefs/blob/v0.5.14/consul/consul.go#L21-L22)). `Renew` returns `ErrLeaseExpired` once the server-side session is gone ([L298-L311](https://github.com/superfly/litefs/blob/v0.5.14/consul/consul.go#L298-L311)). *Read in source.* A sleeping primary simply finds its session gone on wake. Expiry is decided by the coordinator, not by the sleeper's clock. *Inferred.*
- I did not find a primary source for a sleep-aware object-store lock in rclone or kopia, and did not pursue them further.

## 3. Go on macOS

- `runtime.nanotime` on darwin calls `mach_absolute_time` through libSystem and scales it by `mach_timebase_info` ([sys_darwin.go#L304-L321](https://github.com/golang/go/blob/go1.26.3/src/runtime/sys_darwin.go#L304-L321), [sys_darwin_arm64.s#L145-L147](https://github.com/golang/go/blob/go1.26.3/src/runtime/sys_darwin_arm64.s#L145-L147), [sys_darwin_amd64.s#L90-L92](https://github.com/golang/go/blob/go1.26.3/src/runtime/sys_darwin_amd64.s#L90-L92)). The wall clock is `clock_gettime(CLOCK_REALTIME)` ([sys_darwin_arm64.s#L139-L143](https://github.com/golang/go/blob/go1.26.3/src/runtime/sys_darwin_arm64.s#L139-L143)). *Read in source.*
- Apple: `mach_absolute_time` "does not increment while the system is asleep" ([developer docs](https://developer.apple.com/documentation/kernel/1462446-mach_absolute_time)). `mach_continuous_time` increments "including while the system is asleep" ([developer docs](https://developer.apple.com/documentation/kernel/1646199-mach_continuous_time)). *Documented.*
- So **Go's monotonic clock stops during sleep on macOS.** The `time` package docs say "On some systems the monotonic clock will stop if the computer goes to sleep" ([time.go#L54-L59](https://github.com/golang/go/blob/go1.26.3/src/time/time.go#L54-L59)). *Documented.*
- **Timers fire late.**
  - Runtime timers compare their `when` against `nanotime()` ([runtime/time.go#L484](https://github.com/golang/go/blob/go1.26.3/src/runtime/time.go#L484)). *Read in source.*
  - Because `nanotime` stops during sleep, a `time.Timer`, `time.Ticker`, `time.Sleep` or `context.WithTimeout` that was due during the sleep fires after the remaining *awake* time. It fires late by about the length of the sleep. *Inferred.*
  - [golang/go#35012](https://github.com/golang/go/issues/35012) ("NewTimer firing later if computer sleeps") reports exactly this on macOS. It was closed as a duplicate on 2025-10-08. *Documented.*
  - [golang/go#66870](https://github.com/golang/go/issues/66870) ("macOS monotonic time paused during sleep") was closed with a pointer to the docs. Ian Lance Taylor confirms the value comes from `mach_absolute_time`. *Documented.*
  - The general fix is the still-open proposal [golang/go#36141](https://github.com/golang/go/issues/36141) ("add ExternalNow, etc for external time and timers"). [golang/go#24595](https://github.com/golang/go/issues/24595) covered Linux `CLOCK_BOOTTIME`. *Documented.*
- **A clock that counts sleep and does not jump exists.**
  - macOS `clock_gettime(3)`: `CLOCK_MONOTONIC` "will continue to increment while the system is asleep". `CLOCK_UPTIME_RAW` does not, and equals `mach_absolute_time` ([Libc clock_gettime.3#L67-L84](https://github.com/apple-oss-distributions/Libc/blob/71bbe350ab79eef58113991d817ccc6165061a64/gen/clock_gettime.3#L67-L84)). *Documented.*
  - `golang.org/x/sys` v0.42.0, already in `go.mod`, has `unix.ClockGettime` and `CLOCK_MONOTONIC` for darwin ([zsyscall_darwin_arm64.go#L1065](https://github.com/golang/sys/blob/v0.42.0/unix/zsyscall_darwin_arm64.go#L1065), [zerrors_darwin_arm64.go#L230](https://github.com/golang/sys/blob/v0.42.0/unix/zerrors_darwin_arm64.go#L230)). *Read in source.*
  - Measuring the lease with it would count sleep without the backwards-jump risk of the wall clock that [#559](https://github.com/djosh34/s3-smb/issues/559) worried about. *Inferred.*

## 4. Mapping restic's approach onto s3-smb

### What is in the bucket after a takeover

Server A sleeps. Server B (another server ID) takes over. All of this is *read in source*.

- **B's lock key `lock/<B>/<run>`.**
  - B puts it, lists, and proceeds only if A's key is at least 10 minutes older than B's own key in that listing ([lock.go#L68-L92](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L68-L92), [#L106-L126](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L106-L126)).
  - While B runs, it re-puts the key every minute ([lock.go#L131-L152](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L131-L152)).
  - If B crashes, the key stays and goes stale ([README.md#L192-L194](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/README.md#L192-L194)).
  - If B exits cleanly, it **deletes its own key**, if its lease is still valid ([engine.go#L562-L565](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L562-L565)).
- **B's start copy.**
  - Every start opens a new timeline with a new random history ID ([engine.go#L227-L256](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L227-L256)).
  - It uploads `db/<highest+2>-<counter>-<history>`, where `highest` is the highest sequence B listed or recorded ([engine.go#L197-L219](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L197-L219)). `Open` does not return until that copy has landed.
- **B's deletes come only after that.** `makeCopy` runs `cleanup` only after the upload has landed ([copies.go#L114-L142](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L114-L142)). That is where copies and trashed chunks are deleted.
- **Nobody deletes another server's lock key.**
  - `takeLock` deletes only its own key while it waits ([lock.go#L94](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L94)). Shutdown deletes only its own key.
  - Dead keys are left alone ([#627](https://github.com/djosh34/s3-smb/issues/627)).
  - Keys of the same server ID are skipped, not deleted ([lock.go#L118](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L118)).
- **Today's renewal only re-puts its own key and never lists** ([lock.go#L139](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L139)). The lease check is `passed()`, the larger of the monotonic and wall clocks, in `check()` before every commit and S3 write or delete, and in `watch` every second ([engine.go#L436-L457](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L436-L457), [#L586-L601](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L586-L601)).

### Why restic's check does not transfer

restic's evidence is a deletion of the sleeper's own lock. Deletion is final, and taking over requires it. In s3-smb, takeover is passive: B waits for staleness. A's key is never deleted, so "my key still exists" is always true and proves nothing. B's own key can vanish again through a clean exit. **The lock folder alone cannot prove that nobody took over.** *Inferred.*

### The smallest safe check

*Inferred design. No code is proposed.*

Trigger: the lease lapsed by a clock that counts sleep. Today `check()` and `watch` would then call `fail(errLeaseExpired)`. Instead, the run would enter a held state, with these rules:

0. **Hold.** No SQLite commit and no S3 PUT or DELETE may start, including attempts inside SDK retries that are already running. Reads may continue.
1. **PUT** this run's own lock key, the same key as before. Note the send time.
2. **LIST `lock/`.** Stop for good if any key of another server ID is fresh against this run's key under the existing `freshHolder` rule: Last-Modified within 10 minutes, by the backend's clock.
3. **LIST `db/`.** Stop for good if any copy has a sequence above `captureSeq`, the highest sequence this run has started to capture ([copies.go#L148-L157](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L148-L157)).
4. If both checks pass, set the lease to the send time of step 1 plus 8 minutes and release the hold. If either fails, stop as today. launchd restarts the server and `Open` runs the full start.

**The race it must close.** A woken A lists at the same moment that a starting B passes its own put-and-list.
- Both sides put before they list. Assume LIST shows every PUT that completed before the LIST began.
- If A's LIST missed B's key, then B's PUT landed after A's LIST, so after A's PUT. B's LIST then sees A's key with a Last-Modified only seconds older than B's own key. B treats A as fresh, deletes its key and waits.
- So at most one of them proceeds. This is the same argument as `takeLock`'s comment ([lock.go#L59-L67](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L59-L67)).
- A false positive is possible: A sees a waiting B's short-lived key and stops. That only costs a restart.

**Facts that make it safe:**

1. B can do nothing else in S3 until its put-and-list has seen A's key 10 minutes stale ([lock.go#L75-L92](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L75-L92)). *Read in source.*
2. B's first object other than its lock key is its start copy, numbered above A's `captureSeq`. *Inferred:*
   - A has at most one copy in flight (`copyMu`).
   - If that copy had landed when B listed, B's start copy is at least `captureSeq + 2`. Otherwise it is `captureSeq + 1`.
3. B deletes nothing until that copy has landed ([copies.go#L126-L142](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L126-L142)). *Read in source.*
4. The highest sequence in `db/` never goes down. *Inferred:*
   - Cleanup keeps the 4 newest sequences ([copies.go#L195-L207](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L195-L207)).
   - The late-copy sweep only removes `(startSeen, startSeen+2]` and spares the running server's own start copy ([copies.go#L183-L191](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L183-L191)).
   - So B's trace survives a clean exit, a crash and later servers.
5. A's own earlier runs cannot cause a false alarm. Their late copies are at most `startSeen + 2`, which is at or below A's `captureSeq`. *Inferred.*
6. LIST consistency.
   - AWS documents strong read-after-write for PUT and DELETE ([S3 user guide](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel)). *Documented.*
   - That LIST shows every completed PUT is assumed, as `takeLock` already assumes. Borg's docstring states the same need and says AWS and MinIO meet it. *Inferred.*

**Facts that make it unsafe, or holes I found:**

1. **SDK retries already in progress bypass `check()`.**
   - `check()` runs once per call ([engine.go#L470-L482](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L470-L482)). The SDK then retries inside that call for up to 6 minutes ([bucket.go#L66-L104](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/bucket.go#L66-L104)). *Read in source.*
   - A trash DELETE or copy PUT whose backoff was running at sleep sends its next attempt after wake without any check. *Inferred.*
   - Today the same race exists: `watch` cancels the loops' context within about 1 s of wake, while the retry timer may fire first. *Inferred.*
   - The hold therefore has to stop write attempts at the attempt level, the way restic's `Freeze` stops everything except lock files. Or the run cancels every in-flight write on wake and redoes it after the check.
   - The `get` retry loop in `bucket.go` is a read and does not matter here.
2. **Detection must happen in the write path, not only in `watch`.**
   - After wake, whichever Go timer is due first fires first: `renewLoop`, `copyLoop`, an SDK backoff or `watch`. *Inferred from section 3.*
   - `renewLoop`'s next PUT must not count as a renewal after a lapse. The restic race above shows the risk.
3. **The clock.** `passed()` already counts sleep through the wall clock, but a wall clock set backwards hides a sleep. `CLOCK_MONOTONIC` on darwin avoids that (section 3). *Inferred.*
4. **A late start copy from a crashed B is not covered.**
   - Case: B takes over, crashes while its start copy is in flight, and that PUT lands only after A's check passed. A never notices.
   - A's later copies may share or sit below that sequence. A later start could pick B's copy as newest ([pickNewest, engine.go#L337-L353](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L337-L353)). That would roll back A's backups.
   - This needs a takeover, a crash and a request landing after its 6-minute client timeout. It belongs to the documented "one request in flight" limit ([#559](https://github.com/djosh34/s3-smb/issues/559)).
   - A cheap second net: `cleanup` already lists `db/` every 15 minutes ([copies.go#L176](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L176)). It could stop on any copy above `captureSeq` before it deletes anything. *Inferred.*
5. **Cleanup today trusts foreign copies.** `cleanup` takes the 4 newest sequences from the full listing, including other servers' copies ([copies.go#L195-L200](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L195-L200)). If A ever ran cleanup after a takeover, B's newer copies would raise A's "oldest kept", and A would delete trash that B's restored database may still use. *Read in source; consequence inferred.* This is why the check must come before any delete.
6. **Same server ID on two machines.** `freshHolder` skips keys of this run's own server ID ([lock.go#L118](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L118)). If the data folder is copied to another Mac, for example by Migration Assistant, step 2 sees nothing. Only step 3 catches it. This hole exists today and has nothing to do with sleep. *Inferred.*
7. **Commits must stay held.** A commit lets s3-smb tell the Mac that data is safe. If commits ran during the hold and the check then failed, the restart would restore B's copy and lose them. *Inferred.*
8. **The 30-minute copy rule has the same sleep problem.** `watch` stops the engine when `passed(newest + 30 min)` ([engine.go#L596-L598](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L596-L598)), and `passed()` uses the wall clock. Any sleep over 30 minutes therefore ends the run on wake, even with the lock fixed. With no network, it also ends the run after 30 awake minutes. *Read in source.*

## 5. No network

Versions are as in `go.mod`: aws-sdk-go-v2 v1.41.5, smithy-go v1.24.2, Go 1.26.3.

### How the client is set up

*Read in source* ([bucket.go#L77-L104](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/bucket.go#L77-L104)):

- `retry.NewStandard` with `MaxAttempts = 1<<20`, `MaxBackoff = 20 s` and `RateLimiter = ratelimit.None`.
- Each call gets `context.WithTimeout(6 min)`.
- `http.Transport` sets `ResponseHeaderTimeout` 1 min and `TLSHandshakeTimeout` 30 s. It has no `DialContext`, so Go uses a zero `net.Dialer` with no connect timeout of its own ([transport.go#L1303](https://github.com/golang/go/blob/go1.26.3/src/net/http/transport.go#L1303), [#L1320](https://github.com/golang/go/blob/go1.26.3/src/net/http/transport.go#L1320)).

### Which errors are retried

*Read in source.*

- A failed HTTP send is wrapped in `RequestSendError`, which reports `ConnectionError() == true` ([smithy-go client.go#L101-L110](https://github.com/aws/smithy-go/blob/v1.24.2/transport/http/client.go#L101-L110), [#L131-L139](https://github.com/aws/smithy-go/blob/v1.24.2/transport/http/client.go#L131-L139)).
- `RetryableConnectionError` checks for a `*net.DNSError` **first** ([retryable_error.go#L101-L113](https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/aws/retry/retryable_error.go#L101-L113)):
  - `IsNotFound` → **not retryable**.
  - `Temporary()` → retryable.
  - Otherwise the switch below decides, where a connection error or dial error is retryable ([#L115-L160](https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/aws/retry/retryable_error.go#L115-L160)).
- A non-retryable error returns at once ([middleware.go#L253-L257](https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/aws/retry/middleware.go#L253-L257)).

### DNS on macOS

- Go on darwin uses the system resolver, libSystem `getaddrinfo` ([conf.go#L178-L181](https://github.com/golang/go/blob/go1.26.3/src/net/conf.go#L178-L181)). *Read in source.*
- `EAI_NONAME` and `EAI_NODATA` become "no such host" with `IsNotFound = true` ([cgo_unix.go#L189-L190](https://github.com/golang/go/blob/go1.26.3/src/net/cgo_unix.go#L189-L190), [net.go#L649](https://github.com/golang/go/blob/go1.26.3/src/net/net.go#L649), [#L684-L709](https://github.com/golang/go/blob/go1.26.3/src/net/net.go#L684-L709)). `EAI_AGAIN` is `Temporary` ([cgo_unix.go#L36](https://github.com/golang/go/blob/go1.26.3/src/net/cgo_unix.go#L36)). *Read in source.*
- **Not established:** which code macOS `getaddrinfo` returns when there is no network at all. I found no Apple primary source.
  - If it is `EAI_NONAME`, the S3 call fails in milliseconds with no retry. That is fast, but a short DNS failure then fails a request at once.
  - If it is `EAI_AGAIN`, the call follows the retry path below.
  - This needs a Mac test.

### No route, refused, or DNS answered from cache

- `connect` fails at once with a dial `*net.OpError`, which is retryable. *Read in source; error kind inferred.*
- The delay before retry *n* is `rand[0,1) × 2^n` seconds while `n ≤ int(log2(20)) = 4`, then a flat 20 s without jitter ([jitter_backoff.go#L23-L48](https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/aws/retry/jitter_backoff.go#L23-L48)). *Read in source.*
- With `ratelimit.None` there is no retry quota. 1<<20 attempts are never reached. *Read in source.*
- The call ends only when the backoff sleep sees the 6-minute context end, which returns a `RequestCanceledError` ([middleware.go#L288-L295](https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/aws/retry/middleware.go#L288-L295)). *Read in source.*
- That is about 4 quick attempts in the first ~15 s, then one every 20 s, roughly 20 attempts in all. *Inferred.*

### Connect that hangs

This covers Wi-Fi without internet, or packets dropped. There is no dial timeout, so each attempt waits for the OS TCP connect timeout, then the TLS limit of 30 s and the header limit of 1 min. Fewer attempts fit in the 6 minutes. The stop is still at 6 minutes. *Inferred.*

### All budgets stop during sleep

The 6-minute context and the SDK backoff are Go timers, so they count awake time only (section 3). A request that is pending at sleep keeps its remaining budget after wake. *Inferred.*

### Effects on s3-smb with no network while awake

*Read in source; timeline inferred.*

- `renewLoop` blocks in a PUT for up to 6 minutes. Since the lease ends 8 minutes after the last successful send, the engine stops and s3-smb exits about 8 minutes after the network went away. This also happens with no sleep at all ([lock.go#L131-L152](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/lock.go#L131-L152), [engine.go#L586-L601](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/engine.go#L586-L601)).
- `makeCopy` retries the same copy every 10 s after each 6-minute failure ([copies.go#L114-L125](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/engine/copies.go#L114-L125)). The 30-minute rule then stops the engine if the lease has not already done so.
- Each SMB request that needs S3 waits up to 6 minutes before the Mac gets an error. Interim replies keep it alive meanwhile ([async.go#L15-L19](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/internal/smb/server/async.go#L15-L19)).

## 6. macOS behaviour

### (a) Time Machine when the destination is unreachable or a backup fails

- "Time Machine saves one snapshot of your startup disk approximately every hour, and keeps it for 24 hours. It keeps an additional snapshot of your last successful Time Machine backup until space is needed." When the backup disk isn't available, Time Machine uses local snapshots to restore files ([Apple support 102154](https://support.apple.com/en-us/102154)). *Documented.*
- Time Machine "performs hourly, daily, and weekly backups" ([Mac User Guide](https://support.apple.com/guide/mac-help/back-up-your-mac-with-time-machine-mh35860/mac)). *Documented.*
- **No primary source found** for:
  - when Time Machine retries after a failed or unreachable network backup;
  - how a failure mid-backup is handled;
  - whether backups run in Power Nap dark wakes on this hardware.

  `backupd` is not in Apple's open source. The Apple support pages on Power Nap that I tried redirected to a battery page with no Time Machine detail. I did not guess.

### (b) The SMB client across sleep and wake

All from SMBClient tag [SMBClient-538.100.12](https://github.com/apple-oss-distributions/SMBClient/tree/SMBClient-538.100.12).

- **Sleep and wake hooks.**
  - The kernel extension registers for sleep and wake. On `kIOMessageSystemWillSleep` it only sets `gSMBSleeping = 1`. On `kIOMessageSystemHasPoweredOn` it clears that and records `gWakeTime` with `nanouptime` ([smb_sleephandler.cpp#L40-L70](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_sleephandler.cpp#L40-L70)). *Read in source.*
  - `gSMBSleeping` is only declared elsewhere, for example [smbfs_subr_2.c#L40](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/smbfs/smbfs_subr_2.c#L40). I found no code that reads it. *Read in source.*
  - Nothing tears down sessions or fails requests at sleep. *Inferred from the above.*
- **Request timeouts count awake time.**
  - A request's send time is taken with `nanouptime` ([smb_iod.c#L1250-L1252](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_iod.c#L1250-L1252)). A `STATUS_PENDING` interim reply resets it ([#L2127-L2128](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_iod.c#L2127-L2128)).
  - A request times out with `ETIMEDOUT` 2 minutes after that (`SMB_SEND_WAIT_TIMO`, [smb_conn.h#L759](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_conn.h#L759), [smb_iod.c#L3131-L3150](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_iod.c#L3131-L3150)).
  - A share request also breaks the connection for reconnect when nothing has been received for `session_resp_wait_timeout` and the request is that old too ([smb_iod.c#L3074-L3107](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_iod.c#L3074-L3107)). That timeout is 35 s, or 45 s with leasing ([smb_conn.c#L796-L800](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_conn.c#L796-L800), [smb_smb_2.c#L6815](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_smb_2.c#L6815)). An echo goes out after 10 s of silence (`SMBUETIMEOUT`, [smb_conn.h#L777](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_conn.h#L777)). *Read in source.*
  - `nanouptime` reads `mach_absolute_time` ([xnu kern_time.c#L853-L863](https://github.com/apple-oss-distributions/xnu/blob/f6217f891ac0bb64f3d375211650a4c1ff8ca1ea/bsd/kern/kern_time.c#L853-L863), [arm/rtclock.c#L290-L300](https://github.com/apple-oss-distributions/xnu/blob/f6217f891ac0bb64f3d375211650a4c1ff8ca1ea/osfmk/arm/rtclock.c#L290-L300)), and that clock stops during sleep (section 3). *Read in source.*
  - So a request outstanding at sleep does not time out because of the sleep. Its 2 minutes continue after wake from where they stopped. s3-smb's interim timer is also a Go timer that stops during sleep, so the two stay in step. *Inferred.*
- **A reconnect that a sleep interrupted starts over.**
  - It gets a fresh start time and wait time after wake ([smb_iod.c#L3487-L3500](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_iod.c#L3487-L3500), [#L3788-L3800](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_iod.c#L3788-L3800)).
  - A blocked socket receive resets its 15 s limit to the wake time ([smb_trantcp.c#L585-L591](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.100.12/kernel/netsmb/smb_trantcp.c#L585-L591)). *Read in source.*
- **Over loopback** the TCP connection does not depend on a network interface, so the session should simply continue after wake. *Inferred.*
- README says macOS mounts with a new client GUID "after ... a long sleep" ([README.md#L113](https://github.com/djosh34/s3-smb/blob/1518da308d735299cbb9391e0d4409830e932423/README.md#L113)). I did not find the code path in SMBClient that makes that happen after a sleep. It may live in user space (NetFS or the automounter), which I did not read.

## Open points

1. Measure on a Mac what `getaddrinfo` returns with Wi-Fi off and with Wi-Fi on but no internet. The SDK fails at once on "no such host" and retries "try again". The answer decides whether no-network requests already fail fast.
2. Decide how the hold answers SMB requests while S3 is unreachable: fail fast, or wait. It must not answer with a commit.
3. Decide how to stop write attempts that the SDK is already retrying when the Mac sleeps (hole 1 in section 4). The options are an attempt-level gate, like restic's `Freeze`, or cancelling in-flight writes on wake.
4. Decide what the 30-minute copy rule should count, given that it also ends the run after a sleep over 30 minutes and after 30 minutes without network.
5. Decide whether the lease clock moves to `CLOCK_MONOTONIC` through `x/sys/unix`, or keeps the larger of the monotonic and wall clocks.
6. Decide whether `cleanup` should also stop on a copy above `captureSeq`, as a second net against a late start copy from a crashed server.
7. The same server ID on two machines, for example a data folder moved by Migration Assistant, is invisible to the lock check today. Only the copy check would catch it.
8. Backends other than AWS (Garage, B2, R2) need list-after-write consistency for both `takeLock` and the proposed check. This is not verified here.
9. There is no primary source for Time Machine's retry timing after a failed network backup, or for Time Machine in Power Nap. A Mac log capture (`log show --predicate 'subsystem == "com.apple.TimeMachine"'`) would answer it, as observation rather than source.
10. The restic refresh race described in section 1 was found by reading only. I did not check restic's tracker for it.
