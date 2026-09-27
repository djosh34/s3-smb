# s3-smb implementation contract

Status: draft for approval. This specifies application work, not agent coordination. No application has been implemented. The [wayfinding map](https://github.com/djosh34/s3-smb/issues/1) remains the decision index. [Implement the first s3-smb release](https://github.com/djosh34/s3-smb/issues/18) has ten implementation sub-issues with native dependencies, tests and review requirements.

[Decide credential refresh, first-run key handling, and license grant](https://github.com/djosh34/s3-smb/issues/17) holds the remaining user choices. [Approve the implementation-ready s3-smb backlog](https://github.com/djosh34/s3-smb/issues/29) blocks every implementation task until shared understanding is confirmed.

Two choices remain for the user: credential refresh and initial encryption-key handling. The user approved `AGPL-3.0-only`. Defaults below that depend on the remaining answers are marked pending. The rest are concrete implementation defaults for review, not new claims of user approval. The [four-reviewer audit](reviews/plan-audit.md) is complete. Its accepted technical corrections are included below and in the implementation issues. These are planning corrections, not implemented fixes.

## Release scope

- A foreground Go executable named `s3-smb`. Install without checkout using `go install github.com/djosh34/s3-smb@<version>`.
- One SMB share and account, backed by embedded JuiceFS, supported SQLite metadata, S3 object storage and the native bounded cache. No FUSE, external database service or separately installed third-party native libraries. A normal C compiler and platform SDK are allowed.
- Linux/ARM64 is the first automated acceptance platform. Preserve the pinned SMB implementation's Time Machine protocol support. Darwin build validation and the user's later Mac/Time Machine test are separate evidence, not implied by Linux tests.
- The remote dataset may exceed available local storage. Cache eviction must not delete remote data or make evicted files inaccessible.
- Default bind is `127.0.0.1:445`. Explicit `0.0.0.0` and an unprivileged test port work. Explain permission errors for privileged ports; do not install services or change machine privileges.
- Authentication is required. No anonymous share. No GUI, service manager, daemonization, mDNS requirement, backup scheduler for clients, custom browser or agent orchestration.
- Exactly one writable metadata authority may use a dataset. A local exclusive lock prevents two processes using the same state directory. This is not a cross-machine lease. Before recovery, the user must stop the old writer. Concurrent independent SQLite authorities are unsupported.

## Source and build

Use module path `github.com/djosh34/s3-smb` with its executable at the module root. Pin the tested Go toolchain in CI and document its minimum supported version.

Copy the required pinned JuiceFS and customized dependency implementations into ordinary packages under the root module. Include the required go-smb2 source closure where project-owned protocol corrections are needed. Preserve imports' meaning, algorithms, licenses and notices. Keep a source manifest with upstream URLs, revisions and each intentional local patch. No effective `replace`, workspace, ordinary vendor directory, nested module, build overlay or special consumer build tags may be needed for installation.

Pins:

- JuiceFS `v1.4.1`, commit `0b90c7db5a929ae6adc5faad948d108efd2c99f9`.
- go-smb2 commit `277a9300411249a881a05f7a910f5a83ae3395f2`.
- Customized dependencies come from the selected JuiceFS replacement implementations. The packaging research identifies seven in its default embedded graph. Do not substitute upstream SQL/progress implementations merely to compile.

The Linux module-proxy fixture demonstrated untagged versioned installation with bundled JuiceFS/customized dependencies and an externally imported SMB module. The planned project-owned SMB source copy still needs the same build/install checks. The real published module must repeat that test from an empty directory and fresh caches. No upstream requests or release dependency on upstream acceptance.

Confirmed license choice: use `AGPL-3.0-only` for original project code, with full dependency license text, notices and an appropriate source offer. Do not relicense upstream files.

## Command and configuration

Commands:

```text
s3-smb serve [-c PATH] [--log-format text|json] [--read-only]
s3-smb --help
s3-smb --version
```

Accept `-c` before or after `serve`. There is no `init` command or silent `--yes` behavior. Help/version do not load credentials, execute helpers, contact S3 or create state. Return zero after a clean signal-driven shutdown or a declined confirmation. Return nonzero for configuration, storage, recovery, listener and protection failures.

Use one strict YAML config. Its default location on both platforms is `$XDG_CONFIG_HOME/s3-smb/config.yaml`, falling back to `$HOME/.config/s3-smb/config.yaml`. Reject unknown or duplicate fields. Resolve relative file paths against the config directory. Do not perform shell interpolation. Missing config is an actionable error, not a wizard.

Configuration groups:

| Group | Fields and rules |
| --- | --- |
| `smb` | `listen`, `share`, `username`, `password`. One share/account. Password may be a literal in the private YAML. Validate nonempty names/password and address before opening the listener. |
| `storage` | `volume`, `state_dir`, `cache_dir`, optional `cache_size` as a byte quantity with KiB/MiB/GiB units. Use the volume's native object namespace consistently. State defaults under `$XDG_DATA_HOME/s3-smb`, otherwise `$HOME/.local/share/s3-smb`. Cache defaults under `$XDG_CACHE_HOME/s3-smb`, otherwise `$HOME/.cache/s3-smb`. Inherit documented pinned native cache defaults, not workload-size assumptions. |
| `s3` | `bucket`, `region`, optional absolute `endpoint`, optional `path_style`, `access_key`, `secret_key`, optional `session_token`, and `tls`. An explicit region avoids region-discovery assumptions for compatible providers. Custom endpoints require an explicit scheme. AWS endpoints use HTTPS. Never silently downgrade a custom host to HTTP. Explicit HTTP is available for intentionally unencrypted/test endpoints. Reject credential-bearing endpoint user-info, queries and fragments. |
| `s3.tls` | Optional `ca_file`, plus paired `client_cert_file` and `client_key_file`. Add private CA roots to the process's copy of the system root pool, never to the operating system trust store. Client authentication is independent of custom-CA use. Load these files at startup; replacing them takes effect after restart. Hostname and certificate verification stay enabled. No insecure-skip-verify option. |
| `encryption` | `passphrase`, using a value/file/command source. Whether a separate `private_key_file` is needed depends on the pending key-location choice below. Use native JuiceFS object encryption, initially `aes256gcm-rsa`. No new data-encryption protocol or key-rotation product. |
| `backup` | `interval`, default `1h`; `timeout`, default `30m`; `trash_days`, default `14`. Reject disabling protection and enforce the timing inequality below against the actual native format retention. Apply valid trash-retention changes through native format configuration before maintenance, then read them back. Use native metadata-export retention. This does not promise that every old export still has all its data blocks. |
| `logging` | `format`, default `text`, or `json`; `level`, default `info`. The CLI format flag overrides YAML and selects logging even for malformed-config failures. |
| root `read_only` | Default false. The CLI flag can enable it. Read-only mode allows local cache/recovery work but never modifies remote objects or the served namespace. It does not initialize a new dataset. |

Create private state/staging directories with mode 0700 and secret files with mode 0600. Existing YAML containing literal secrets, credential/passphrase files and private-key files must be owned by the effective user and have mode 0400 or 0600. Validate the opened file, not a path checked before opening. Fail without printing contents when these rules are not met. Public CA/certificate files and a config containing only protected-file references are not secret files.

Credential programs run with the daemon's privileges; they are not sandboxed. Do not accept an untrusted config as safe merely because the subprocess avoids a shell. Document explicit config/state paths for elevated execution and keep bind failures actionable. The daemon never changes host privileges or widens its bind address automatically.

Remote encryption does not encrypt SQLite, its WAL, cached file data or temporary metadata exports. Native format-field wrapping is not local-disk encryption. Protect local state with permissions and explain when the user should use operating-system disk encryption.

### Credential sources

Each S3 access key and secret key independently selects exactly one source. Optional session tokens use the same representation.

```yaml
s3:
  access_key:
    file: /path/to/access-key
  secret_key:
    command: [program, argument, another-argument]
```

The third form is `value: "literal credential"`. Source combinations are independent. Reject zero or multiple sources, empty command arrays and empty resolved required values. Remove one terminal LF or CRLF from file/command output, not arbitrary whitespace. Reject NUL bytes and oversized output.

Execute the argument array directly, never through an implicit shell or command-string parser. Supply no interactive stdin. Capture stdout privately and keep stderr out of application output. Use a bounded helper lifetime, initially 30 seconds, a 64 KiB output limit and bounded pipe cleanup. Do not log argv, stdout, stderr, raw execution errors or secret values. File/command failures do not fall back to another source. Helper paths and arguments come only from the user's config, never SMB requests or remote metadata.

Pending refresh choice: resolve at startup and retain those values until restart. This question concerns S3 access credentials, not certificate rotation. Ordinary access keys can remain valid until replaced or revoked; temporary session credentials expire. Do not imply that expiring credentials work indefinitely. If uninterrupted rotation is required, revise this contract with an explicit refresh/session-token policy before implementation.

### Initial key handling

Pending user choice: retain only the passphrase as the encryption secret by keeping a protected key in S3, or retain a protected key file separately. The first option is now recommended. Native JuiceFS still uses a random RSA key internally; the passphrase unlocks its stored bytes. Do not derive RSA keys from passwords. S3 access and nonsecret endpoint/bucket/volume settings remain separately necessary. Optional mutual TLS has its own client credentials.

If the user selects the S3-held key:

1. After confirmed initialization, generate a random RSA key and encode it as authenticated encrypted PKCS#8 using the already selected GMSM dependency. Do not use JuiceFS's legacy `ExportRsaPrivateKeyToPem` encryption or the PKCS#8 library's implicit defaults. The proposed profile is RSA-3072, PBES2 scrypt with `N=131072, r=8, p=1`, a 16-byte random salt, and AES-256-GCM with a 12-byte nonce and 16-byte tag. Measure its approximately 128 MiB KDF working allocation and unlock time on Linux/ARM64; do not silently weaken the profile.
2. Put the encrypted PEM at `<volume>/recovery-key.pem` through the raw S3 store. It must not pass through the JuiceFS wrapper that needs that same key to decrypt it. Keep it outside native `meta/` rotation and `chunks/` cleanup. Never replace an existing key on retry.
3. Fetch it back, unlock it and verify that it is the generated key before allowing writable service. The initial consistent metadata backup is still required. A timeout after a possibly accepted upload requires inspection, not key regeneration.
4. On recovery, fetch and unlock this object before opening native encrypted backups. Reject missing/corrupt/mismatched keys without remote mutation. No separately retained PEM or local key file is required. An independent copy is optional, not another supported key-management mode.
5. Bound the downloaded PEM to 64 KiB and inspect its standard encoding before password derivation. Accept only the selected encrypted profile and key size; reject plaintext, legacy PEM, excessive KDF costs and unsupported parameters before allocating KDF memory. Verify ciphertext authentication and sanitize failures.

Source inspection supports this format and native-parser route; the actual PKCS#8/S3 recovery round trip still needs tests. Anyone with the encrypted key object can try offline password guesses. Recommend a long random passphrase. Loss of that passphrase or all copies of the protected key is unrecoverable. Do not imply that S3 access credentials replace the passphrase.

If the user instead selects external-key recovery, specify its protected PEM path and retention instructions and adjust tests accordingly. Do not implement both workflows by default. No password-manager integration or custom recovery archive is required.

## Module interfaces

Keep four application-owned modules behind small interfaces. Names below describe responsibilities, not required Go identifiers.

1. Config parses and validates settings without side effects, then explicitly resolves secret sources for startup. Callers receive typed settings/errors, not YAML nodes or subprocess output.
2. Storage owns the native filesystem, metadata connection, object store, encryption, cache, required message callbacks and maintenance lifetime. It exposes opening/closing, native filesystem access, remote-state classification, confirmed initialization, backup and recovery operations. Mutation/cleanup stays stopped until startup protection succeeds. Do not expose raw database transactions to the CLI or SMB code.
3. The SMB adapter implements the pinned `VFSFileSystem` and `ByteRangeLocker` interfaces over storage. It owns handles, directory enumeration positions and lock cleanup. SMB protocol policy remains in the pinned server with narrow tested corrections.
4. The foreground application owns startup decisions, confirmation, listening, scheduling and shutdown. Logger and clock/failure seams support deterministic tests; do not add generic provider/plugin frameworks.

The storage lifecycle reports backup completion/failure as results, not by parsing log text. Only one scheduled backup runs at once. Use native maintenance callbacks and tasks explicitly rather than assuming a FUSE/CLI entry point started them. Document which mutable workers must stop/join before closing their resources. Harmless native process-lifetime loops may end with the foreground process; do not promise that every native constructor becomes an independently restartable library.

## Startup and shutdown

1. Select logging, parse/validate config, resolve credentials and load TLS settings. Acquire the local exclusive state lock before touching mutable metadata.
2. Classify remote state for the configured dataset. Distinguish known empty, recognized existing, unknown/nonempty and inspection failure. Permission errors or missing local SQLite do not mean new remote state. Compare local/remote volume identity when local metadata exists. Never format over a mismatch, corrupt database or unknown objects.
3. With no local metadata and genuinely empty remote state, ask whether to initialize. Before mutating anything, recheck state. If it changed, stop. A failed/partial initialization must remain distinguishable from a new empty dataset and must not silently format again.
4. With no local metadata and an existing dataset, offer the newest native metadata backup that passes decryption, decompression, format and import validation. Display its timestamp and warn about lost later changes and the need to stop any old writer. If the latest candidate is corrupt, do not silently choose an older point; report it and require explicit confirmation of the older candidate. Wrong keys, no readable backup or an unknown format are errors. Say that metadata restoration is not a full data-content check. Do not call a sample proof that every file survives; missing referenced objects must produce explicit read errors, never empty-file success.
5. Read confirmation from `/dev/tty`, separately from log streams. Proceed only on explicit `y`/`yes`, case-insensitive. Other answers decline or reprompt; EOF declines. If confirmation is needed without a controlling terminal, fail promptly. A normal already-initialized restart must not need a terminal.
6. Native recovery imports into a fresh temporary SQLite database. Validate the loaded format/volume and complete the import before publishing the database as active. A failed import must not leave a database that the next startup mistakes for healthy state. Preserve the source backup and remote dataset. Current validated YAML owns credentials, endpoint, bucket, region, path style and TLS throughout bootstrap/startup/recovery. A saved format must not redirect credentials, disable TLS verification or load old local certificate paths. Reject destination/identity conflicts rather than silently following the dump.
7. Initialization and recovery require a successful new consistent encrypted metadata backup before writable listening or destructive maintenance. An ordinary restart may reuse a confirmed successful backup for the same volume whose snapshot age is less than the configured interval and whose remote object still matches the recorded result. Persist that small success receipt atomically in private local state only after upload completes; native `lastBackup` is an attempt marker, not success evidence. A missing/invalid/old receipt requires a new backup before serving. Reuse must preserve the original next-due time, so repeated restarts cannot postpone protection indefinitely.
8. Start the native maintenance required by the selected embedded graph, the backup schedule, and the SMB listener only after their prerequisites hold. In read-only mode, do not start mutation, backup upload or destructive cleanup tasks.
9. On SIGINT/SIGTERM, close the destructive-work gate, stop accepting connections, drain/cancel requests, flush and close tracked handles, stop/join mutable workers, close the metadata session and SQL engine, then release the state lock. Bound the foreground process's shutdown to 30 seconds. Native close/flush calls can wait longer and some native loops cannot be canceled. Add narrow cancellation where needed; on the hard deadline exit nonzero rather than releasing the lock while a writer remains alive. Do not claim every buffered write survives an unclean kill.

A failed scheduled backup immediately closes the destructive-work gate and starts shutdown with nonzero exit. Preserve the previous usable backup. A stuck or overdue backup is a protection failure, not success. No multi-day grace period follows a known failure.

### Remote-state classification

Inspect the entire configured volume prefix without modifying it. Use the native `<volume>/juicefs_uuid` marker as identity evidence, not as the sole emptiness test. Recognized native areas are `chunks/`, `meta/` and `testing/`, plus the proposed `recovery-key.pem`. Unrecognized objects are never deleted.

- A successful complete listing with no objects and no existing local dataset is the only new-dataset case.
- A recognized metadata backup means existing state even if the identity marker is missing. Validate the decrypted format's name/UUID against configuration and any existing marker; never initialize over it.
- Marker-only, key-only, testing-only or chunks-without-backup states are nonempty/partial, not new datasets. Resume only with matching intact local initialization state and the same key/UUID; otherwise fail with an explanation and leave the prefix unchanged.
- Listing failure, conflicting UUIDs and nonempty state with no recognized identity or recovery path are errors. Existing local metadata plus an unexpectedly empty remote prefix is not permission to reformat.

Initialization must publish and verify the native identity marker through the native encrypted store; do not copy the CLI's warning-only marker-write behavior. Only the proposed passphrase-protected bootstrap key uses raw S3 access. The marker and protected-key object are never replaced by a retry. After recovery, a missing marker can be repaired only from the validated recovered UUID, after confirmation, and never in read-only mode.

### Backup timing and deletion gate

Let `I` be the interval, `T` the total backup-operation timeout and `S` the shutdown bound. A backup operation gets at most three attempts with 1-second and 2-second delays, all inside `T`; underlying network retries also consume that same deadline. Do not stack an unbounded outer retry around native retries.

Schedule the next operation at the later of the prior snapshot start plus `I` and the prior operation's completion. Never overlap operations. The conservative maximum age of the protected snapshot is `B = max(I, T) + T`. Require the effective native trash duration to be strictly greater than `B + S`. A delayed operation gets no extension past the current point's protection deadline. Validate clock anomalies rather than treating a future timestamp as fresh forever.

The native retirement/deletion paths, including already queued work, must consult the live protection gate before irreversible work. `NoBGJob` and the application timer are insufficient: native deletion workers and read-triggered compaction can still run. On failure or expired protection, refuse retirement/deletion, retain the ability to retry safely, and terminate. Test suspension with cleanup released before the backup callback. The check must constrain the actual deletion cutoff and use snapshot-start time; a delayed callback must not authorize stale work merely because a later upload completed.

## Filesystem and SMB correctness

Implement every required operation in the pinned `VFSFileSystem`: open/create/close, attributes, filesystem statistics, positional read/write, truncate, flush/fsync, directories, lookup, rename/unlink, links and extended attributes. Honor handle identity when a path is renamed or unlinked. Return an explicit supported error for a genuinely unsupported operation; never silently acknowledge a mutation.

- Offset I/O uses native positional methods. Validate integer conversions, ranges and short reads/writes. EOF is not an I/O failure. Sparse writes and truncation must round-trip.
- Directory continuation/restart and bounded entry counts must work without duplicates caused by losing cursor state. Define behavior under concurrent mutations using native semantics.
- Names resolve within the exported filesystem, including symlinks and `..`. No host-filesystem escape. Preserve Unicode and the pinned SMB case behavior rather than invent a filename-normalization layer.
- Use a stable unprivileged native identity for the SMB account and configure its root-directory ownership explicitly. Do not use `meta.Background()`'s UID 0 for client operations. Reserve native trash against SMB purging or moving retained entries out, including through resolved aliases/handles. Hiding `.trash` from listings alone is not protection. Ordinary deletion must still use native trash.
- Preserve native attribute precision and macOS-related extended attributes/resource-fork behavior. A binary xattr is data, not a log string.
- Use logical native filesystem statistics, not local cache free space as remote share capacity.
- Implement byte-range locking, including conflict, unlock, blocking/cancellation behavior and close/disconnect cleanup. Do not inherit the optional no-op fallback. Ensure locks apply across handles/sessions and interact correctly with I/O.
- SMB FLUSH and the resource-fork/xattr WRITE path discard backend errors at the pin. Correct both, and inspect CLOSE for the same class of problem. The adapter receives the WRITE flags and must honor `SMB2_WRITEFLAG_WRITE_THROUGH` by completing the native flush before returning success. A backend mutation error must not acknowledge bytes as stored.
- Centralize the narrow server error mappings. Check `ENOENT` to name-not-found, `EACCES`/`EPERM`/`EROFS` to access-denied, `ENOSPC` to disk-full, `EBADF` to invalid-handle and storage I/O failure to I/O-device-error. Preserve the existing lock-conflict mapping and operation-specific statuses where the protocol requires them. Wire tests must name expected statuses; an adapter errno alone is insufficient.
- Keep native writeback disabled. Successful FLUSH/write-through must mean native remote data upload and local metadata synchronization completed. Configure SQLite WAL with `_synchronous=FULL` on every connection, including restored databases; the current driver otherwise defaults to NORMAL. Native file Fsync alone does not upgrade SQLite durability. Test the effective pragma and synchronization-error propagation. A SIGKILL restart test is not a host-power-loss test.
- Ordinary buffered WRITE success does not promise an S3 metadata recovery point. Periodic metadata recovery can lose later changes as already agreed. Do not alter unrelated negotiation/authentication/Time Machine behavior.

Every local upstream correction needs a regression that fails without that correction and a source-manifest entry.

## Metadata protection and recovery

Use native metadata JSON export, gzip staging, encryption, upload and load. Do not upload live SQLite files or invent a streaming backup format. Replace the native helper's shared `/tmp/meta` and default file modes with application-owned 0700 staging and 0600 temporary files. Remove only application-owned temporary exports after success/failure and on a safe subsequent startup.

The native helper ignores `gzip.Close` errors. The extracted operation must check export, gzip finalization, staging I/O and upload results before recording success or rotating backups. Inject failure specifically while writing the gzip trailer; successful JSON export alone is not enough. Disk-full/export/upload failures preserve the prior point and stop protection-dependent serving.

Never upload to an existing backup key. Native names have only second precision. Under the one-writer rule, check for collisions and wait for an unused native timestamp within the operation deadline or fail safely; do not overwrite. Treat a lost upload response as ambiguous, not permission to reuse that name. Keep the actual snapshot time separately for age checks. Test rapid restarts, backward clocks and an accepted upload whose response is lost.

The research found that fast whole-filesystem SQLite JSON export uses one snapshot. Slow export can mix transactions during compaction. Native automatic backup switches paths around 100,000 inodes and can skip above 1,000,000 at hourly cadence. The embedded path must explicitly use the consistent SQLite export for every supported namespace size and remove the silent-skip behavior. Keep the native format and encryption. Extract a small result-returning native backup operation if needed; do not run a second native scheduler alongside the application schedule.

Test export while writes, renames, deletions and native compaction run. Restoring the result must produce a coherent snapshot, not mixed data mappings. Test threshold selection without requiring every normal test run to create a million files. Include a slower realistic namespace test and record peak memory, staging space, duration and write contention. The fast native export materializes metadata in memory. Supporting remote data larger than the cache does not mean unbounded metadata needs no RAM or staging space. Fail safely on resource exhaustion and document measured requirements; do not invent a workload-size cap or silently switch to inconsistent export.

Keep native trash and cleanup behavior, with the startup/age gates above. Metadata-export retention does not pin data objects. Reject configurations whose timing makes the last successful point unsafe before another backup can finish or the daemon can stop. Do not promise a full 14-day history. Require users not to apply external lifecycle rules that delete live JuiceFS objects. Older retained exports may no longer be recoverable.

Native writable same-dataset recovery already passed the recorded local-object probes. Do not redesign allocation counters based solely on reused IDs. Repeat the tested sequence over S3: save point A, mutate/compact after A, remove local state, restore A, resume writes, then verify restored contents and a new recovery point. Never start another writer concurrently.

## Logging and secret handling

Use a single standard `slog` handler on stderr. In JSON mode, ordinary application, SMB, JuiceFS, Xorm and AWS SDK diagnostics are JSON records. Prompts use the controlling terminal. Do not promise JSON for unrecovered Go/C/runtime crashes or a failed output destination.

Use the existing SMB/JuiceFS hooks and the small adaptations identified by [the logging research](https://github.com/djosh34/s3-smb/blob/df35d9cbd617e4b6e24f0b3a7c82767c7d5ee8cf/docs/research/embedded-logging.md): bridge Xorm before its first ping, supply the SDK logger during client/region configuration, disable progress, account for the progress writer reset, and route the recovered export-panic stack through slog. Avoid duplicate records and recursive bridges. Preserve native fatal/panic behavior.

Never log whole configs, passwords, S3 credentials/tokens, keys/passphrases, helper output/argv, SQL arguments, signed request bodies or xattr values. Sanitize error paths before they reach the logger. Test with synthetic secret markers at debug level and capture both stdout and stderr. JSON encoding alone is not redaction.

## Acceptance and review

The implementation sub-issues carry their own tests. These release-level checks are mandatory:

1. Unit and regression tests pass with the pinned Go toolchain; application concurrency tests pass under the race detector. Each intentional upstream patch has a failing-before/fixed-after regression where practical.
2. Linux/ARM64 tests exercise the real executable and SMB client against an isolated S3-compatible test service, not only an in-memory filesystem or local object backend. The normal install needs no test container tooling. Integration setup pins a maintained ARM64-capable S3 fixture and records its version. No cloud account or production data is needed for the automated suite.
3. End-to-end tests cover all nine access/secret source combinations, optional token, custom endpoint/path-style routing, private CA acceptance, wrong-CA/hostname rejection and mutual TLS.
4. Protocol tests cover authentication, file/attribute/directory operations, locking, FLUSH/write-through errors, read-only rejection and malformed/overflowing requests without a server panic.
5. Cache tests write and verify data larger than the configured cache, force eviction, restart without the old cache, and verify S3 refetch. Include bounded local disk pressure and staging failure. The design must not require a full local replica.
6. Recovery tests remove every application state/cache/config/key file from the first installation. Recreate only the materials required by the selected key-location choice. For the proposed S3-held-key path, these are the passphrase, S3 access and nonsecret connection details, not an externally retained PEM. Restore over S3, verify every expected file in the test fixture, resume writes and make/restore another backup. Test wrong secrets, missing/truncated backups, missing referenced data and interrupted imports without remote overwrite. Metadata-load success must not mask missing-data errors or be presented as full content verification.
7. Fault tests cover S3 outage during flush, backup export/upload failure, deadline expiry, overdue scheduling, a process crash/restart, startup after downtime and signal shutdown. Verify nonzero exit when protection fails and preservation of the prior point. Use deterministic failure seams and controlled clocks where wall-clock sleeps would make tests unreliable.
8. Clean public `go install github.com/djosh34/s3-smb@<published-version-or-commit>` succeeds with `GOWORK=off`, no checkout, fresh module/build caches, no consumer build tags and only the permitted native toolchain. Record the exact revision and command. A local proxy proof is not this release test.
9. A native Darwin build runs on a macOS runner with its normal SDK, or remains an explicit unmet release check. Do not report a successful CGo cross-compile from Linux as native Mac evidence. Record actual loopback/wildcard bind results and port ownership/permission errors on the target OS; never widen the bind automatically. The later user-run Time Machine test must interrupt a backup, discard local daemon state, restore the earlier metadata point, read older files, resume backup and restore a file from the new backup. Use a disposable dataset with no other writer. Generic mount/reconnect tests do not prove this scenario.
10. Code review checks the accepted spec and repository standards separately, plus durability, recovery/cleanup ordering, locking, secret exposure, upstream patch scope and license/source distribution. Record reviewed revision, commands, findings and fixes. Do not mark a task complete with unresolved data-loss or secret-leak findings.

Record basic SMB listing, cold/warm read and sequential write measurements with the fixture and workload, plus metadata-export resource measurements. These are baseline evidence, not invented throughput/latency promises or another sizing questionnaire.

The Linux milestone may be complete before the user's Mac test. Release documentation must name the exact tested platforms/provider fixture and outstanding checks. It must not claim general Time Machine or every-provider compatibility from source inspection.

## Research evidence

- [Verify SQLite-only CGo, native caching, and go install feasibility](https://github.com/djosh34/s3-smb/issues/3) links the embedding/cache findings. The later bundled-CGo decision supersedes the original strict native-library constraint in its title.
- [Verify encrypted metadata backups and fresh-install recovery](https://github.com/djosh34/s3-smb/issues/5) links native export/encryption/load findings, snapshot tests and retention limits.
- [Verify native writable recovery to the same S3 dataset](https://github.com/djosh34/s3-smb/issues/10) links the tested native same-dataset restore sequence. Its object backend was local, not S3.
- [Verify versioned installation with bundled upstream source](https://github.com/djosh34/s3-smb/issues/13) links the source layout and successful untagged Linux module-proxy fixture. It did not install a completed public application.
- [Verify slog integration and JSON-only embedded logging](https://github.com/djosh34/s3-smb/issues/16) links the source findings and bounded Linux logger/TTY probes.
- The [historical handoff's source findings](smb-s3-time-machine-handoff.md#smb-source-findings-and-required-fixes) identify the pinned SMB flush bug and no-op-lock fallback. Historical product proposals elsewhere in that file are not approvals.

## Completion rules

Each implementation issue must include code, tests, relevant docs/source-manifest updates, and a recorded code review. Tests exercise public module behavior rather than only private helper calls. Fixes stay within this contract; newly discovered contradictions return to the plan instead of silently changing recovery guarantees or scope.

The plan is approved only after the remaining decisions are resolved and the user confirms the resulting backlog. Until then, implementation issues remain blocked. No agent ownership, scheduling, messaging, worktree, merge or coordination scheme belongs in this plan.
