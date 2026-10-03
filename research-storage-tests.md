# Triage: storage/cache/recovery and tests/CI/harness issues (s3-smb, main @ ac712a6)

Read-only research. I read all 25 issues (#64 only the first post and the last comment). I spot-checked the code, workflows and run history. Severity is judged for a backup product: critical = silent loss or corruption; high = failed backup or leak; medium; low.

## 1. Storage / cache / recovery / metadata

| # | Title (short) | Category | Severity | Rec. | Size |
|---|---|---|---|---|---|
| 91 | After recovery, a retained disk cache returns old bytes for new files | recovery+cache | **critical** (new writes read back wrong, S3 holds the right bytes; reproduced 3 times) | FIX: wipe or re-generation the cache dir whenever recovery runs (`serve.go` ~L207-233, before `OpenFilesystem` L305). Keeping the dumped counters (see #92) is not enough, because IDs allocated after the backup also sit in the cache. Add the populated-cache recovery test from #116 | S-M |
| 92 | Restored pending deletions can delete a new file that reuses the inode | recovery/meta | **critical** (delayed silent loss of post-recovery files) | FIX: in `loadEntries` (`internal/juicefs/pkg/meta/dump.go` L447+), set `counters = max(rebuilt, dm.Counters)` for NextInode/NextChunk. Today counters start at 2/1 and ignore the dumped high-water marks. Same root cause as #91, one PR | S |
| 128 | Race between `cachedStore.upload` and `diskCache.flush` | cache | high (data race on page bytes that go to the disk cache; content checks passed so far) | FIX: the unmerged `origin/swarm/64-page-pool-v3` (0db3360) is small: it moves `bcache.cache()` after Compress. It also has the zstd `Decompress` cap fix for #131, a separate open issue that may silently corrupt reads | S |
| 144 | Disk-cache `scanned` flag read without the mutex | cache | low (bool race, nonzero cache only) | FIX: `origin/swarm/64-takeover-cache-scan` (f024cc84), +7 lines in disk_cache.go plus a test | S |
| 114 | Recovery drops delslices records, so compacted objects leak forever | recovery/meta | high-ish (permanent S3 cost leak on each recovery, no GC, lifecycle expiry discouraged). No data loss | FIX narrowly: export and import `delslices` plus slice refs in the dump. Fallback: document the leak and add a GC later | M |
| 102 | Shutdown with an open handle exits 1 and loses a pending delete | lifecycle | medium (deleted file comes back after restart, exit 1; no data loss) | FIX: in `resources.close` (`serve.go` L42-60), close protection only after `server.ShutdownContext` and `adapter.Shutdown`. The `Manager.Run` cancel path closes protection too and needs the same change. Test both shutdown reasons with open handles | M |
| 59 | GetAttr flushes buffered writes while holding the global FS lock | smbfs perf/locking | medium, rising to high under #140 (slow S3 holds the global `FS.mu`, every SMB request stalls, the client may time out or disconnect) | FIX: report the writer's in-memory length instead of flushing in `attr()`, then narrow the lock around I/O. Also fixes #96 | M-L |
| 113 | ReadDir/Lookup sizes omit acknowledged buffered writes | smbfs | medium | MERGE-INTO #59 (same in-memory-length mechanism) | (S on top of #59) |
| 58 | `storage.capacity` setting | config/storage | low (the rc.8 1 TiB free clamp fixed band size; S3 use is still unbounded and TM never thins for space) | FIX later: pass a JuiceFS format capacity, applied on each start like trash_days | S |
| 60 | Judge metadata-backup and S3 time limits against measurements | measurement | low | MERGE-INTO #121 (measurement). #142 covers the slow-S3 side (chunk Get/Put 60 s, MaxRetries 10) | - |
| 61 | `backup-names/` grows by one empty file per backup | housekeeping | low (~8.7k files/yr, harmless) | FIX: at Manager start, prune reservations older than the oldest retained dump | S |
| 64 | "short client packet header" disconnect | SMB transport | high (failed backup; 2/10 on rc.7, ~0/40 since rc.8) | DROP / close as superseded by #140 (#135 durable handles, #142 chaos). The investigation was stopped by the owner with no cause found. It left 49 swarm refs, 37 runs and artifacts that expire on Oct 10/17. Separate fixes found there: #128, #131, #144 and others | - |

## 2. Tests / CI / harness / docs / robustness

| # | Title (short) | Category | Severity | Rec. | Size |
|---|---|---|---|---|---|
| 140 | Robustness parent: survive drops, slow S3, errors; never crash | robustness epic | high (owner's top priority) | FIX (umbrella; children #135, #136, #137, #141, #142) | L |
| 141 | Strict lint in CI (golangci-lint, shellcheck, actionlint, py) | CI | medium (stops regressions of the ignored-error and panic classes) | FIX, early: a ratchet config, our code + smb2 first | M (config S, reaching zero L) |
| 142 | Chaos tests: flaky network and flaky S3 | tests | high (only way to prove #135 and the slow-S3 behaviour) | FIX | L |
| 143 | Rewrite the Mac TM harness smaller | harness | low (no product impact) | FIX/SIMPLIFY, and absorb #115, #117, #119 | M-L (needs ~45-60 min Mac runs) |
| 115 | Re-run failed Mac jobs looks up wrong `run_attempt` artifact names | CI | low | MERGE-INTO #143 (or a 4-line fix: drop `-${{ github.run_attempt }}` from transfer artifact names, or use `run_id`) | S |
| 117 | Mac scenarios don't assert remote chunks changed during the interrupted phase | harness | medium (weakens the recovery evidence) | MERGE-INTO #143 (assert `chunks[1] > chunks[0]`; machine-loss returns before the 2nd count, `acceptance.py` L594-599) | S |
| 119 | Mac `test_manifest.py` not run in CI; docs say "every test" | CI/docs | low | MERGE-INTO #143 (moot if Python goes away), else add to test.yml | S |
| 116 | No test fills a positive cache, forces eviction, checks the refetch | test gap | medium (would have caught #91) | FIX together with #91, one focused e2e test | S-M |
| 120 | No two-handle / two-connection tests; fakes hide adapter mismatches | test gap umbrella | medium | MERGE-INTO the individual defect fixes (#84/#85/#86/#89/#90/#96/#102), each with a real-adapter regression; multi-connection load goes to #142 | - |
| 121 | Memory/time on a large namespace not measured | measurement | low-medium (possible OOM on big datasets, not shown) | FIX later (absorb #60): one benchmark of peak RSS and export/recovery time | M |
| 125 | Protocol tests miss lifecycles, late-cancel races, byte layouts | test gap umbrella | low | MERGE-INTO the defect fixes (#95/#98/#107/#109/#110) | - |
| 126 | Integration builds the daemon without `-race` | CI | low | FIX: `go build -race` in `test/run-linux.sh` L7 (or a separate race job) | S |
| 127 | Docs omit the SMB subset, the 64 KiB named-stream cap, ignored birth time | docs | low | FIX after #135/#85/#95 settle what is supported | S |

## 3. Test infrastructure now

- **Unit tests**: `go test ./...` covers our packages (app 12 Test funcs, config 22, backup 10, storage 12, smbfs 16, logging 5), smb2 (35 files / 77 funcs), and 10 vendored juicefs/xorm test files. The e2e tests skip without `S3_SMB_E2E_ENDPOINT`.
- **e2e** (`test/e2e`, 9 files, ~2.1k lines, 20 tests): they run the built binary against MinIO over signed SMB and cover recovery, cold recovery, crash, compression, sparse files, protection, startup failures, and an S3 fault proxy (`fault_proxy_test.go`, which can hold a chunk response and fail metadata requests). The positive-cache test writes only `smoke.txt`.
- **Linux integration**: `scripts/test-linux.sh` builds `test/Dockerfile` (Go 1.26.3 plus MinIO built from source at a pinned commit), starts MinIO on a private network, and runs `test/run-linux.sh`. That script runs `go mod tidy -diff`, a plain `go build`, then `go test -race -count=1 -timeout=30m ./...`. e2e takes ~203 s, the whole job ~8 min.
- **CI workflows**:
  - `test.yml`: `go vet` + `go test`, ubuntu-24.04, ~35-45 s. It runs on push to every branch and on PRs, so PRs get duplicate runs. There is no lint beyond vet.
  - `integration.yml`: runs `scripts/test-linux.sh` on ubuntu-24.04-arm, timeout 45 min. Triggers are PR, push to main, and workflow_dispatch. It takes ~8 min every time because MinIO is rebuilt with no layer cache.
  - `macos.yml`: workflow_dispatch only, on macos-15-intel. Inputs are `public_version` (default v0.1.0), mode discover|acceptance|scenarios, and a scenarios JSON list. Acceptance = backup → recover on a fresh Mac, plus a matrix of 5 scenarios, plus machine-loss-recover: 9 Mac jobs, about 5 at a time (one scenario waits for a runner).
- **Mac harness size**: `acceptance.py` 783, `native.py` 157, `manifest.py` 57, `test_manifest.py` 97, `run.sh` 43, `build.sh` 55, `fixture/` (Go, ~small). That is ~1,190 lines plus the 266-line workflow.
- **Can agents run the Mac workflow on a branch?** Yes: `gh workflow run macos.yml --ref <branch> -f mode=acceptance -f public_version=...`. The #64 swarm did exactly this from swarm/* branches. **Catch**: `build.sh` does not build the checkout. It runs `go install github.com/djosh34/s3-smb@$PUBLIC_VERSION` from proxy.golang.org, so only the harness and workflow come from the branch. To test branch product code, push the commit and pass its canonical pseudo-version (`v0.1.1-0.<ts>-<sha12>`). This should work: the repo is public and the version regex accepts pseudo-versions. The other way is to change the harness so it builds the checkout.
- **Duration and reliability**: full acceptance 43-63 min wall (main run 37098439018: 43 min), scenarios-only 49-97 min, discover ~5 min. Since rc.8 (Oct 2 23:46), 7 of 8 non-swarm runs were green; the one failure was server-kill-cold-midpoint in 37084526985. On Oct 2 before rc.8, 4 of ~13 completed runs failed, 2 of them the #64 disconnect. Scenarios allow one resumed-backup retry. Swarm runs of modified harnesses mostly failed for diagnostic reasons.

## 4. What #140/#141/#142 propose

- **#140** (owner's top priority, 2026-10-03): this matters more than finding the root cause of #64. Goals: a dropped connection does not end a TM backup (#135, real durable handles plus DH2C/DHnC reconnect; today the server advertises them and then closes opens on disconnect); slow or failing S3 makes a backup slower, not failed, with the SMB side keeping the client connected while S3 waits or retries; no ignored errors (#136); no client-triggered panic (#137, #87). Children: #135, #136, #137, #141, #142. The owner wants a design session on #135 (wayfinder skill) before building.
- **#141**: a checked-in strict `golangci-lint` config: errcheck incl. blank and type assertions, staticcheck, govet (all except fieldalignment), gosec, errorlint, contextcheck, exhaustive, gocritic, unused, unparam, forbidigo banning `panic`, and others. Measured findings: own code 68, smb2 485, juicefs 815. Also shellcheck, actionlint and a Python linter (only if the harness stays Python), all failing CI. Exceptions need `//nolint:<linter> // reason`, enforced by nolintlint. Scope is own code + smb2 first, with juicefs/thirdparty excluded by path. CI fails on new findings from PR 1 (ratchet), then counts go down in small PRs.
- **#142**: chaos tests. Mainly in the Linux Docker suite (per PR or nightly), with an occasional Mac variant. SMB side: latency, jitter, loss, reordering, duplication, bandwidth caps, mid-I/O resets, multi-second stalls, outages. S3 side: slow headers and bodies, timeouts, bursts of 5xx and throttling, mid-transfer resets, unreachable periods. Kill and restart under chaos. Candidate tools: tc netem, iptables, a cutting TCP proxy, extending the existing fault proxy, dnctl/pfctl on the Mac. Invariants: no crash, no ignored failure, no acknowledged write lost or changed, failures visible, reconnect works once #135 lands, cold recovery after chaos stays consistent, other connections keep being served. Seeded and replayable.

## 5. Dependencies / ordering

1. **Land now (small, no deps)**: #128 + #131 (page-pool-v3), #144 (cache-scan branch), then #91+#92 in one recovery PR with the #116 populated-cache recovery test. These are the only critical/silent-corruption items in scope.
2. **#141 early**: the ratchet config is cheap and gives #136/#137 their enforcement. Python lint depends on the #143 language choice, so decide #143 first or skip Python lint.
3. **#135 design → #142**: chaos tests prove reconnect. Build the #142 SMB/S3 fault proxies before or alongside #135. Slow-S3 chaos will expose #59's global lock, so do #59 (absorbing #113 and #96) in the #140 track. #126 (race-built daemon) is a natural part of the #142 harness.
4. **#143** absorbs #115, #117 and #119. Do it before adding the Mac chaos variant of #142. A green full run (~1 h) is needed to merge.
5. **#102, #114, #61, #58**: independent. #114 touches the same dump/load code as #91/#92, so it fits right after that PR.
6. **Umbrellas #120 and #125** are closed by per-defect regressions. **#60** goes into #121. **#64** is closed in favour of #140. #127 comes last, after the supported SMB subset is settled (#135/#85/#95).

Side note: I ran `git fetch` for two swarm refs to read their diffs. That updated remote-tracking refs only; no branch, issue or code was changed.
