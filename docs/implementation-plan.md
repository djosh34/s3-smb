# s3-smb implementation contract

Status: approved and execution authorized by the user's explicit start instruction on 2026-09-29. Implementation and acceptance are in progress; authorization is not evidence of completion. The [wayfinding map](https://github.com/djosh34/s3-smb/issues/1) records decisions; the [implementation backlog](https://github.com/djosh34/s3-smb/issues/18) tracks delivery. [Final approval](https://github.com/djosh34/s3-smb/issues/29) records agreement with the plan, not completed implementation or tests.

This contract specifies required behavior, how the application uses JuiceFS, and necessary corrections. Follow the pinned upstream implementations for everything else. Do not turn descriptions of native internals into new application subsystems or rigid designs. The [review findings](reviews/plan-audit.md) and [complete comment record](reviews/reviewer-comments.md) retain the supporting evidence and alternatives.

## Product and command

- A foreground executable named `s3-smb`, installed without checkout using `go install github.com/djosh34/s3-smb@<version>`.
- One SMB share/account. The account must have a password. Keep the native normal authentication/signing path rather than enable guest mode.
- Default bind `127.0.0.1:445`; explicit `0.0.0.0` and custom ports work. Never widen binding automatically.
- `serve` uses one YAML config with `-c` override accepted before or after the subcommand. Default path is `$XDG_CONFIG_HOME/s3-smb/config.yaml`, otherwise `$HOME/.config/s3-smb/config.yaml`, on both platforms. Help/version have no credential, network or initialization side effects.
- No separate `init`. `serve` asks before initializing a genuinely new dataset or recovering existing remote state. Keep prompts separate from logs; fail clearly when required confirmation has no controlling terminal. Normal restarts require no terminal.
- Support an optional read-only mode. No GUI, daemonization, service installer, FUSE mount, client backup scheduler or custom backup browser. Test scripts may start and stop the foreground process.
- Linux/ARM64 is the main development platform. Real Time Machine backup and recovery on GitHub-hosted macOS is a required final acceptance task, not a substitute for earlier Linux tests.

## Configuration requirements

Use strict YAML, clear errors, documented defaults and paths relative to the config file where appropriate. Keep one source of configuration truth rather than duplicating options across files.

| Area | Required settings and behavior |
| --- | --- |
| SMB | Listen address, share, account and required password. Native protocol/signing limits must be documented and tested, not hidden by reporting successful authentication. |
| Local storage | State directory, native cache directory and optional `cache_size`. Use XDG data/cache defaults. Omitted capacity uses the native default; explicit `0` must not be replaced by that default. |
| Units | Public size inputs, help and status use decimal units: 1 MB = 1,000,000 bytes; 1 GB = 1,000,000,000 bytes. Convert correctly at the native interface. A small positive capacity must not accidentally become zero because of unit conversion. |
| S3 | Bucket, region, custom endpoint, `path_style`, independent access/secret sources and optional static session token. Explicit `path_style: true` and `false` must reach the S3 client and work in tests; omission may use native selection. |
| TLS | HTTPS verification by default, custom CA file and optional client certificate/key. Add roots to the process trust pool, not OS trust. Certificate-file replacement takes effect after restart. Never silently downgrade verification or an endpoint to HTTP. Explicit HTTP is available for intentional local/test endpoints. |
| Encryption | `enabled`, default true, and a passphrase source when enabled. Explicit false opts out of application encryption of both data and remote metadata backups. |
| Metadata protection | Hourly native metadata backups and 14-day native trash by default, both configurable. Apply retention to the actual native volume format, not just a local field. |
| Logging | Standard `log/slog`, text or JSON, level and a CLI format override usable even for config errors. |

Resolve permanent S3 credentials once at startup. Access key and secret key each independently accept exactly one of `value`, `file`, or direct `command` argv with stdout as the value. No implicit shell, silent fallback or automatic credential renewal. Use bounded helper execution/output, noninteractive stdin and private output capture. Reuse that resolver for the encryption passphrase only when encryption is enabled.

Create new secret/state files with private permissions. Existing ownership or permission modes, including departures from 0400/0600, produce warnings rather than startup rejection. This interprets the user's "modes" request as Unix file permissions. An unreadable file, failed helper or invalid credential is still an error. Public certificates are not secret files. Never print secrets in warnings. Helpers run with the daemon's privileges; avoiding a shell is not sandboxing.

## How to use JuiceFS

Embed the supported SQLite metadata backend, native filesystem, native S3 store and native cache directly. Register and own the lifecycle work normally started by the native CLI, without importing a FUSE/CLI entry point. Keep the native filesystem behavior and implement the pinned SMB interfaces over it, including meaningful locking and flush behavior. Do not build a second cache, filesystem or backup format.

The remote dataset may exceed available local storage. Explicit cache size `0` must work and be tested with MinIO, restart and remote reads. Native zero disables retained disk and RAM block caches; ordinary I/O buffers/readahead still exist. Preserve that behavior, including ignoring an old disk cache and avoiding a positive-capacity fallback. A repeated read may use an active reader buffer, so cold restart/refetch is the acceptance test rather than demanding an S3 GET for every read. SQLite metadata and temporary export space are not the data cache. With caching enabled, test eviction and refetch without requiring a full local replica.

One writable metadata authority may use a dataset. Use a local state lock and require the old writer to stop before recovery. Do not claim the local lock prevents a separate host using another SQLite database. A distributed lease is not in scope.

## Encryption and metadata terminology

When encryption is enabled, file objects and the entire native metadata export uploaded to S3 are encrypted. They use the same native key, unlocked by the configured passphrase. These are the remote "checkpoints" discussed earlier. An S3 metadata backup is not an unencrypted SQLite file upload.

JuiceFS still needs a random private key internally. Keep a strongly passphrase-protected standard PKCS#8 copy at the documented per-volume S3 bootstrap location, outside native data/backup cleanup and outside the encryption wrapper that needs that key. Use the existing compatible library with an authenticated format and reviewed password-derivation parameters. Do not use the weak legacy exporter/defaults identified in the audit. Validate input/cost bounds before deriving a key. Verify publication/readback before serving, never replace a key on retry, and pass the secret directly to the native parser in memory.

Encrypted fresh-install recovery needs the passphrase, valid S3 access and nonsecret connection/volume details. No separately retained PEM is required. An independent copy is optional. Missing/corrupt keys must fail without regeneration or plaintext fallback.

With encryption disabled, no passphrase or key is required or resolved. Use the native unencrypted store for data, identity and gzip metadata backups. Native chunking/compression still apply. Warn that anyone with sufficient S3 read access can read data and metadata. The user accepts that risk. TLS, S3 authentication, the selected SMB access policy, local permissions and metadata protection remain active. Mode is chosen when the dataset is created; a mismatch must not silently convert or reinterpret existing data.

A SQLite WAL checkpoint updates the live local database. Native object encryption does not encrypt that database/WAL, cached data or the temporary plaintext export before upload. The user confirmed that the concern was the remote metadata backups, not local encryption. Keep supported native SQLite and local staging unchanged; do not add SQLCipher, an encrypted filesystem mount or a custom backup format. Encryption-enabled mode already protects the S3 metadata backups with the same passphrase-protected key as data.

## Startup, protection and recovery

- Inspect remote state before initialization. Failed listing/authentication, missing local SQLite, a missing marker or partial initialization do not establish an empty dataset. Leave unknown objects untouched and never format over existing state.
- Use native volume identity and saved format information. A validated backup can supply identity if a marker is missing. Current validated YAML controls S3 destination, credentials and TLS; an old export must not redirect credentials or weaken verification.
- Recover through native decrypt/decompress/load into fresh temporary SQLite and validate before publishing it as active. Display the selected point and possible loss of later changes. Do not silently fall back to an older point.
- Metadata import is not a full data-content check. Missing referenced data must return explicit errors, not empty-file success. Acceptance tests must verify every expected fixture file.
- Initialization and writable recovery require a successful metadata backup in the selected mode before serving and destructive maintenance. Ordinary restarts may reuse a verified recent successful point without postponing its original schedule. Native attempt markers are not proof of success.
- Use native export/upload/retention/load. Ensure a consistent SQLite export and observable success/failure. Do not silently use the known mixed-transaction path or treat a large-namespace skip as success.
- After bounded retries, scheduled backup failure stops writable serving and exits nonzero. Preserve the previous usable point. A stuck/overdue operation is not successful protection.
- Guard actual native reference retirement/deletion, including queued work, against expired protection. Check after downtime/suspension. `NoBGJob` or an application timer alone is insufficient. Configure deadlines and effective retention so cleanup cannot overtake the last protected point; do not add a multi-day grace period after a known failure.
- Stop accepting work, flush/close handles and stop mutable native work before closing its resources or releasing the state lock. Bound process shutdown and report failure if it cannot finish. Do not rewrite harmless process-lifetime loops merely to satisfy an artificial reusable-library requirement.

Keep native trash and metadata retention. An old metadata object does not guarantee all its data blocks survive. Document external S3 lifecycle hazards. Native same-dataset writable recovery has passed the recorded local-backend probes; repeat it against MinIO and real SMB. Do not redesign allocation counters without a demonstrated failure.

## Necessary corrections and integration tests

The audit found concrete work beyond ordinary native integration. Keep patches narrow, source-attributed and covered by failing-before/fixed-after tests:

- Propagate ignored SMB FLUSH and xattr/resource-fork WRITE errors; inspect CLOSE and honor write-through. Return meaningful native protocol errors rather than false success.
- Implement the native locking interface, handle/connection cleanup and path confinement. Preserve Time Machine-related protocol behavior. Do not restate or replace the entire upstream filesystem implementation.
- Use SQLite synchronization that meets the promised local flush durability; the reviewed default NORMAL mode does not. Verify effective FULL mode and error propagation. A process kill alone is not a power-loss test.
- Prevent SMB clients from purging native trash needed by a protected point, including through aliases/handles. Do not use a privileged native identity for ordinary client operations.
- Check gzip finalization and staging I/O before upload or recording backup success. Use application-owned private staging, not the shared native temp path, and clean up abandoned application files safely. Existing permission problems warn; actual staging failures remain errors.
- Never overwrite an existing or ambiguously uploaded backup name. Test native timestamp collisions and lost responses without requiring bucket versioning.
- Route native SMB/JuiceFS/Xorm/SDK diagnostics through slog, disable stray progress and fix the identified logging bypasses. Preserve native termination behavior. Normal JSON logs must remain valid; runtime/C crashes are not promised to be JSON.
- Never log whole configs, credentials, passwords, keys/passphrases, helper output/argv, SQL arguments or xattr values. Test synthetic markers in both output streams, including failures and warnings.

Select ordinary retry, timeout and internal type details during implementation from native behavior and measured needs. Keep the externally visible guarantees above; do not turn earlier illustrative constants or method layouts into product requirements.

## Testing sequence

### 1. Local VM Docker is primary

Run almost all automated work locally first: the application and MinIO in Docker, with the test runner using the same images, configuration, fixtures and assertions intended for GitHub Linux CI. Use a documented repeatable command. Unit/regression/race tests and the E2E suite must be runnable through this local setup; fast isolated tests may supplement it, not replace actual SMB-to-S3 evidence.

Pin the MinIO fixture and relevant toolchains/images. Tests use disposable volumes, synthetic secrets and no production bucket. Keep logs and failed-run artifacts easy to inspect on the VM. Do not make GitHub Actions the normal debugging loop.

Required E2E coverage:

- Password authentication, explicit wider binding and read-only behavior.
- File/attribute/directory operations, locks, write-through/flush failures and malformed requests.
- Independent credential-source combinations, custom endpoint, forced path-style and virtual-host-style requests, private CA and mutual TLS.
- Decimal capacity conversion, omitted versus zero cache, eviction/refetch and remote data larger than the nonzero cache.
- Encrypted and unencrypted initialization, backup, restart and fresh-install recovery after deleting all original local state/config/cache/keys. Disabled encryption must not run passphrase helpers. Verify every expected fixture file, resume writes and restore another backup.
- Partial initialization, wrong secrets, missing/corrupt backup/key/data, staging-full/finalization failures, upload ambiguity, overdue protection, suspension/cleanup races, crash/restart and bounded shutdown.
- Secret-file permission/ownership warnings that do not prevent operation when files are readable, JSON validity and secret redaction in both modes.

Record basic cold/warm read, listing/write and metadata-export resource measurements. No invented throughput promise or workload-size cap. Keep the native consistent export and fail safely rather than silently weaken correctness.

### 2. GitHub Linux CI repeats the same suite

CI invokes the same checked-in local test entry point. Do not maintain a second E2E implementation, reduce assertions, add CI-only skips or change fixture versions. Upload the same logs/artifacts. Pin what can be pinned and report real environment differences; identical test code does not guarantee identical host timing.

Also validate the real public Linux `go install` command from an empty directory/fresh caches. Native Darwin build/install verification starts the final Mac job rather than a separate early Mac CI run. A local module-proxy fixture is not a completed public-application installation test.

### 3. Actual Mac Time Machine acceptance is last

The user-approved replacement in [#34](https://github.com/djosh34/s3-smb/issues/34) and [#18](https://github.com/djosh34/s3-smb/issues/18) governs this final task. Earlier application/Linux/public-install qualification is retained; harness-only changes reuse a qualified application version without repeating that release cycle. Record harness and application revisions separately. Product changes need appropriate regression/qualification tests. See [Mac acceptance](macos-acceptance.md) for the current procedure.

First remove the superseded exhaustive eligible-source scan, native eligibility parser/path association, capacity estimator/size gate, full-source coverage assertion and whole-backup metadata comparisons, including their obsolete helpers/tests. Do not hide them behind flags or add another planning/comparison framework. Preserve useful native commands, application/Linux tests and historical failure evidence.

The user subsequently authorized a labelled positive control with a synthetic password using qualified rc6 to advance the normal path, then crash/resume. On 2026-10-02 the owner withdrew the requirement for access without a password, so password authentication is the only path. The earlier execution stop rule is rescinded. Keep authentication mode explicit in evidence, avoid an authentication framework, and do not repeat Linux/release qualification for control-only harness changes. See [current execution](macos-acceptance.md#current-execution-labelled-positive-control).

The normal path uses two dependent fresh GitHub-hosted Mac jobs:

1. Create a small known tree on Mac A with multiple files, nested folders and empty folders; retain an independent reference separately. Complete an actual normal full-Mac Time Machine backup through the application-backed SMB share to native local MinIO. Keep native exclusions and exclude only recursion-producing test infrastructure. Do not narrow the source to the known tree or delete installed SDKs, user content or the checkout. After run36640633507 proved ENOSPC, the latest chosen [capacity path](macos-acceptance.md#authorized-ci-only-capacity-relief) is standard `macos-15-intel` with actual free space unknown, using the same qualified rc6 and only post-build cleanup of exact attempt-created disposable roots. The prior unused-Xcode/simulator cleanup proposal is superseded and was not implemented; all installed SDKs remain. Record exact scratch paths/reasons, actual free space, native Time Machine percent/bytes and bounded task footprints, not a source inventory/estimator or capacity guarantee. Earlier two opaque runner deaths are merely consistent with the proven failure, not independently diagnosed.
2. Wait for the native application metadata backup containing the completed Time Machine state. Cleanly stop clients, application and MinIO, then transfer the complete stopped MinIO directory through GitHub Actions artifacts, separately from the reference. The user-authorized future direct-store path renames the stopped directory instead of creating a second local tar copy; ship the existing inventory for each store/reference artifact and use its guard to restore omitted empty directories on Mac B. Include hidden files, no symlink transfer or new manifest framework. The immutable in-flight 68110a9 run still uses tar and is not altered for this preparation; neither future direct transfer success nor current store footprint is yet established.
3. On a second fresh Mac B, freshly install the qualified application, start the same pinned MinIO with that store and recover through documented S3 inputs. Never transfer old daemon-local database/cache/configuration/key files. Use Apple's actual remote Time Machine restore into a fresh output and compare **only the deliberately created tree**: paths, types, empty directories and file contents. No Apple/system/SDK content or strict whole-system metadata audit; no raw-tar equality gate.

Only after that normal path passes, exercise an observed application crash during a later active Time Machine/S3 write, normal recovery and native baseline restore, then resumed backup and native restore of the updated tree. Reuse the same small independent comparisons. Do not handpick/repair an older recovery point. A fixed-sleep idle kill, generic SMB copy, fixture-only backup, local-snapshot restore or successful metadata import is not acceptance.

Use native loopback services and an immutable MinIO revision matching the Linux fixture. No external account, new paid infrastructure or nested VM is required or authorized. Keep real credentials and transient CI authentication out of logs, artifacts and the backed-up source. Report actual disk/artifact/runtime/permission failures without inventing a capacity guarantee or reviving an estimator. Preserve concise native command, completion/recovery-point, handoff and comparison evidence; historical publication ceremony must not block a narrow retry. Source inspection does not establish hosted-runner feasibility. Keep issues open until normal and crash/resume evidence actually passes.

## Source, license and completion

Pin JuiceFS v1.4.1 at `0b90c7db5a929ae6adc5faad948d108efd2c99f9` and SMB at `277a9300411249a881a05f7a910f5a83ae3395f2`. Bundle the necessary source as ordinary root-module packages, preserve upstream implementations/licenses and record only intentional patches. No effective replacements, workspace, vendor dependency, nested module, consumer build tags or upstream release dependency may be needed for installation. Portable bundled CGo with a normal compiler/platform SDK is permitted.

Original project code uses AGPL-3.0-only. Include upstream notices and source-distribution information. Every implementation task needs tests, relevant documentation and review of both repository standards and the accepted spec. Record the tested/reviewed revision and distinguish source inspection from executed evidence.

No unresolved data-loss, secret-exposure or false-compatibility claim is acceptable. The final Mac task is now required; passing Linux alone is not completion of this updated release plan. No agent-coordination scheme belongs here. The user approved this backlog and subsequently released the execution hold on 2026-09-29. Keep unfinished acceptance requirements open until their evidence is recorded.
