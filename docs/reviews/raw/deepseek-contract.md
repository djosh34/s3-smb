# DeepSeek contract audit of the s3-smb implementation plan

Auditor: PI_MODEL=deepseek/deepseek-v4.1-flash, PI_PROVIDER=openrouter, PI_REASONING_LEVEL=max.
Revision audited: `fa36915f6a0de6fdb12b1292cc2102a79349071d`.
Plan issue: https://github.com/djosh34/s3-smb/issues/18. Audit ticket: https://github.com/djosh34/s3-smb/issues/30.

## Scope and method

I read the whole implementation contract at `docs/implementation-plan.md`, `README.md`, `CONTEXT.md`, the audit snapshot at `/tmp/s3-smb-plan-audit/audit-issue.json`, the issue snapshot at `/tmp/s3-smb-plan-audit/issues.json` including the ten implementation sub-issues, and the four research notes listed in the audit request. I inspected the pinned JuiceFS source at `0b90c7db5a929ae6adc5faad948d108efd2c99f9` in the recovery and logging research workspaces, the pinned go-smb2 source at `277a9300411249a881a05f7a910f5a83ae3395f2` in the logging research workspace, and the Go 1.26.3 standard library source. I ran no builds, tests or probes. Statements marked source inspection come from reading that code. Statements about probes come from the recorded research notes. I mark inference as inference.

Settled decisions are not reopened: AGPL-3.0-only, embedded JuiceFS with SQLite and bundled CGo, no FUSE, the direct SMB adapter, hourly metadata backups and 14-day trash defaults, foreground operation with fail-stop on scheduled backup failure, remote `go install`, Linux/ARM64 first, and the four application modules. My assigned emphasis was the unresolved passphrase question, contradictions, hidden assumptions, needless restrictions, security, and coverage of the user requirements.

## Finding 1. The encryption recovery material is framed as a false choice, and passphrase-only recovery is possible through a stored protected key

Severity: high. User decision: yes.

Evidence from the contract. Plan line 57 requires `private_key_file` and `passphrase` and defaults the key path inside the private state directory. Plan line 88 tells the user to retain the PEM and passphrase and says "A password alone cannot restore the dataset." The section then says recovery reads the restored PEM and configured passphrase. The reader is left with either retaining a key file or relying on a passphrase that the plan says is insufficient.

Evidence from the pinned source. `pkg/object/encrypt.go:47-64` generates an RSA private key and writes it with `x509.EncryptPEMBlock` using `x509.PEMCipherAES256`. `pkg/object/encrypt.go:67-113` parses the PEM and uses the passphrase only to unwrap it. `pkg/object/encrypt.go:129-140` wraps each object's random data key with RSA-OAEP. There is no passphrase-derived encryption mode, so the passphrase cannot replace the key. The parser also accepts PKCS#8 encrypted keys. `pkg/object/encrypt_test.go:106-108` covers an RSA key in PKCS#8 with a correct passphrase, an incorrect passphrase and no passphrase. That path uses the gmsm pkcs8 library already in the dependency graph. `cmd/load.go:112` parses PEM bytes from memory with `object.ParsePrivateKeyFromPem`, so fetching a protected PEM object from S3 and parsing it in memory uses the same native path. `cmd/format.go:294` and `cmd/load.go:115` read the passphrase from `JFS_RSA_PASSPHRASE`.

Evidence about the bucket layout. A key object stored at the volume prefix, for example `<volume>/recovery-key.pem`, is not touched by native cleanup. `cmd/gc.go:229` limits garbage collection to `chunks/`, and `pkg/vfs/backup.go` limits export rotation to `meta/`. Source inspection, not a probe. The metadata dump cannot bootstrap the key: `pkg/meta/sql.go:5044-5049` removes only `SecretKey` and `SessionToken` from a dump, and the format's remaining key field is wrapped with an AES-GCM key derived as MD5 of the volume UUID at `pkg/meta/config.go:207-231`, while the dump itself is uploaded through the encrypted object store. Reading the field requires the key that the field helps recover.

Security caveat. Go 1.26.3 `crypto/x509/pem_decrypt.go` marks the legacy PEM encryption deprecated as insecure at lines 95-106 and 188-195. The key derivation at line 82 mixes the password with the 8-byte salt taken from the IV and expands the result with repeated MD5 calls until it fills the cipher key. It has no tunable work factor, and the cipher is CBC without authentication. `ExportRsaPrivateKeyToPem` uses that format. The PKCS#8 parser supports PBES2, which can carry a work factor, and the native parser accepts it.

Failure scenario. A user reads line 88, concludes that the passphrase is the only thing to keep, and follows the default key path inside the state directory. The machine is lost, the state directory is gone, and the user has the passphrase and S3 credentials but no PEM. The encrypted backup cannot be opened. In the other direction, a user who stores a protected PEM in the bucket but lets the implementation generate it with `ExportRsaPrivateKeyToPem` and a human passphrase exposes a key blob protected by a fast MD5-based KDF and an unauthenticated cipher, which is cheap to attack offline.

Smallest correction. Replace the section. State that the passphrase unlocks a PEM and cannot replace it, but a stored protected PEM makes the passphrase the only user-retained secret. Offer two named modes: external PEM plus passphrase as the default, and an optional bucket-stored key at a fixed key outside `meta/` and `chunks/`, fetched during recovery and parsed with `object.ParsePrivateKeyFromPem`. If the bucket mode is used, generate a PKCS#8 PBES2-encrypted PEM with the bundled gmsm library, require a long random passphrase, and document that bucket read access exposes the encrypted key and bucket write access can destroy recovery. Move the default key path out of the state directory or require a verified external copy. Update acceptance test 6 at plan line 159 to match the chosen material list.

## Finding 2. The plan assumes the default port 445 is usable and records no binding test

Severity: medium. User decision: no new product decision now; record a platform check first.

Evidence. Plan line 15 defaults to `127.0.0.1:445`, says an unprivileged test port works, and says "Explain permission errors for privileged ports; do not install services or change machine privileges." The handoff records that no privilege model, loopback SMB setup or Time Machine destination registration has been demonstrated. The supplied materials do not contain primary evidence that the target Linux or macOS configurations require root for 445, and they do not contain primary evidence that Time Machine requires exactly port 445. Both platform restrictions are unverified. The plan anticipates permission errors but does not define a verification step or say what happens if the default port fails on the target platform.

Failure scenario. The Linux milestone passes on an unprivileged high port. The later Mac test tries the documented default. If the bind fails or a client cannot reach the share, the project has no recorded answer for the target platform and the choice between a different port, elevated execution or a forwarding step is made ad hoc. If elevated execution is chosen, `HOME` and the XDG defaults resolve to the elevating account, so config, state and cache defaults move, and a user-writable config passed to a privileged process feeds `command:` credential helpers and `private_key_file` paths to that process.

Smallest correction. Add one platform check to acceptance 9 and the release docs. Record the exact target OS and configuration, whether the unprivileged daemon binds 445, the exact error if it does not, and whether a standard client connects. Until that record exists, mark the privileged-port rule and the Time Machine port requirement as unverified assumptions. Do not adopt a root-run policy, change the default port, or add privilege handling now. If the check fails, bring the recorded result back for a user decision.

## Finding 3. Remote-state classification has no specified marker or inspection algorithm

Severity: high-medium. User decision: no.

Evidence. Plan line 104 requires distinguishing known empty, recognized existing, unknown/nonempty and inspection failure, and comparing local and remote volume identity. Line 105 requires partial initialization to remain distinguishable from a new empty dataset. Neither line names a marker or the set of keys that count as recognized. The native CLI writes `<volume>/juicefs_uuid` only in its create path at `cmd/format.go:591`, after applying the volume prefix at `cmd/format.go:287`. A failed put is only a warning. The emptiness check at `cmd/format.go:585` treats any non-testing key under the prefix as non-empty. The bundled snapshot copies only `pkg/...` directories per `bundled-source-install.md`, so the uuid logic in `cmd/format.go` is not in the embedded code unless the app reimplements it. Native components use distinct key spaces: `meta/dump-*.json.gz`, `chunks/`, and `testing/`. A passphrase-only design adds another key, such as `<volume>/recovery-key.pem`.

Failure scenario. An implementer defines empty as "no objects under the volume prefix" and unknown as "any key not in a short list." A first installation after a failed init, where the bucket holds `juicefs_uuid` but no backup, dead-ends. A dataset whose uuid put failed with only a warning, or a key object from the passphrase-only mode, can be misread as unknown. In the worst direction, an init that failed after uploading data but before writing the marker is treated as new, gets formatted again, and the first new backup is written under a fresh format while the old objects become unreachable.

Smallest correction. Specify the classification rules in the contract. Use `<volume>/juicefs_uuid` as the native identity marker. List the recognized keys: `juicefs_uuid`, `meta/dump-*.json.gz`, `chunks/`, `testing/`, and the optional recovery key object. Never initialize while any non-test key exists under the prefix. When the marker is missing but data or `meta/` objects exist, stop with instructions. Add tests for uuid-only, uuid plus chunks without a backup, data without uuid, and a foreign key.

## Finding 4. The fresh-backup-before-every-writable-start rule is stricter than the agreed failure model, and the cleanup age gate is undefined

Severity: medium. User decision: yes.

Evidence. Plan line 109 requires a successful encrypted backup from the active metadata before writable listening and destructive maintenance, after initialization, ordinary restart and recovery. A failure stops startup. Plan line 113 says destructive maintenance must be gated by a "sufficiently recent" snapshot and to use its snapshot time for age checks. It defines neither the threshold nor the cleanup cutoff. The settled decision in issue 14 says startup after a long shutdown must check protection before enabling cleanup and must stop after a failed scheduled backup. It does not require a new export on every start. Issue 6 accepts loss since the last successful backup. Native cleanup is not snapshot-relative. `pkg/meta/base.go:811` starts `cleanupTrash` with the session. It reads days from the format at `pkg/meta/base.go:3083` and computes a cutoff from the current time at `pkg/meta/base.go:3251`. The app cannot pass a snapshot-relative cutoff without gating that goroutine or patching the path.

Failure scenario. S3 is briefly unavailable or slow at restart. A valid backup from 15 minutes ago exists, but the daemon refuses to serve. On a large namespace every restart pays a full export, and `pkg/meta/sql.go:4808-4838` shows the fast export loads all metadata tables into memory. If the strict rule is dropped without adding the age gate, native cleanup can expire slices older than `trash_days` while the newest successful backup is older than that window, and fresh-install recovery then references deleted objects.

Smallest correction. On an ordinary restart with healthy local metadata, require a successful backup inside a computed age window instead of a new export. Produce a new export after initialization, after recovery, when no usable backup exists, or when the window fails. Before starting native cleanup, compare the newest successful snapshot time to `trash_days` and stop or patch the cleanup cutoff when the snapshot falls outside the window. Define the window from `trash_days` minus interval, timeout, the shutdown bound and a detection margin, and reject configurations where it is not positive.

## Finding 5. Plaintext export staging uses the system temp directory with default permissions, and local state carries wrapped credentials

Severity: medium. User decision: no.

Evidence. Plan line 132 requires private writable staging and protection of plaintext while it exists. Issue 24 acceptance says plaintext staging is private. Native `pkg/vfs/backup.go:103-112` creates the gzip export under `os.TempDir()` plus `/meta/`, uses `os.Create` with the default mode, and creates the directory with mode 0755. On a default umask the file is world-readable. Deferred cleanup removes the file only on a normal return, so a crash leaves it behind. `pkg/meta/sql.go:5044-5049` removes `SecretKey` and `SessionToken` from the dump but leaves the key field. `pkg/meta/config.go:207-231` wraps `SecretKey`, `SessionToken` and the key field with an AES-GCM key derived as MD5 of the volume UUID and stores the UUID in the same record. Source inspection only. I did not run an export.

Failure scenario. On a shared machine another user reads `/tmp/meta/dump-*.json.gz` during or after an export. The file contains the full namespace, attributes and chunk mappings. A copied local SQLite file yields the S3 credentials and the wrapped key field through a key derived from the UUID stored beside it, with one MD5 invocation and no secret input.

Smallest correction. Point the process temp or the helper's staging directory at an app-owned 0700 directory, patch or wrap the helper so it never creates files in the shared temp directory, create files with mode 0600, and remove leftovers on the next start. Document that local state holds recoverable S3 credentials and key material and must be protected like the config. Add a test that asserts the staging path, file mode and cleanup.

## Finding 6. The user's actual recovery scenario is not in the Mac acceptance checklist

Severity: medium. User decision: no, but the user should know this is the gate that matters.

Evidence. Plan line 162 defines the later Mac checklist as mounting, backup, disconnect/reconnect and reading and restoring files. Plan line 140 repeats the writable recovery sequence over S3, but without Time Machine. Issue 6 says later Mac tests must determine whether a saved filesystem state also contains a usable Time Machine backup. The handoff names the key acceptance test as recovering yesterday after deleting every local app state file during today's backup. Plan line 165 allows the Linux milestone to complete before any Mac test.

Failure scenario. All automated tests pass and the project is called release-ready. The user interrupts a Time Machine backup, recovers the outer filesystem to the previous metadata snapshot with the passphrase and S3 credentials, resumes Time Machine, and finds that the sparsebundle is inconsistent or the backup cannot continue. The motivating scenario fails after the plan's gate has opened.

Smallest correction. Add one user-run scenario to acceptance 9. Interrupt a Time Machine backup, recover to the previous metadata point using only external materials, confirm older files are readable, resume Time Machine, complete a new backup, and restore a file from it. Record the result. Before the interrupted-run test, rehearse a read-only import into a throwaway state directory so a failed recovery does not disturb the live dataset.

## Finding 7. The plan has no performance or browsing acceptance, though responsive browsing is a stated user priority

Severity: medium. User decision: yes.

Evidence. The handoff decision table records "Fast, reliable, and responsive backup browsing" as a user priority and says request latency, flush frequency, compaction and cache misses need measurement rather than assumed throughput. The plan, README and CONTEXT contain no performance, latency, throughput or measurement requirement. I searched all three. Acceptance 5 checks cache correctness and eviction, not latency. Acceptance 2 checks protocol behavior with a real SMB client, not responsiveness.

Failure scenario. The correctness suite passes. Browsing an existing Time Machine image with millions of files through SMB and SQLite metadata is slow enough that Time Machine times out or the server is unusable. The first real workload is the user's Mac, after the Linux milestone is declared complete.

Smallest correction. Add one recorded measurement to acceptance 2 or 5 using the real executable and SMB client. Measure sequential write and read throughput on a fixed file set, a repeat read that shows cached behavior, and a listing of a large directory tree. Record the fixture, provider and numbers. If the user accepts a correctness-first release, say that explicitly in the release notes.

## Finding 8. The rule for insecure existing secret files is undefined

Severity: low-medium. User decision: no.

Evidence. Plan line 62 requires owner-only creation and says "Warn or fail appropriately on insecure existing secret-bearing files." It separately requires owner-only permissions for a YAML file containing literal secrets. The private key and passphrase files are secret-bearing, but the plan does not say whether an existing group-readable or world-readable key file warns or fails. The acceptance matrix tests synthetic secret markers in logs, not file modes.

Failure scenario. A world-readable PEM or passphrase file on a multi-user machine produces a warning and the daemon serves. Any local user reads the key and passphrase, which defeats the client-controlled encryption requirement even though the S3-side encryption is correct. If elevated execution is ever chosen, the same check must accept a properly protected file owned by the account that runs the daemon.

Smallest correction. Fail closed for any file whose contents are the private key or passphrase when group or other has read access. Keep the warning only for a restrictive config that references separately protected files. Name the accepted modes and add accept and reject tests.

## Smaller observations

- The plan says at line 31 that the module-proxy fixture demonstrated the source layout. The fixture copied JuiceFS plus seven customized dependencies, kept the SMB server as an ordinary external module, and reused caches, per `bundled-source-install.md`. The plan now copies the SMB server source closure as well. Acceptance 8 will test the real module. Narrow the claim before approval.
- The one-writer rule is documented as a local lock and not a cross-machine lease. Two hosts can still corrupt one dataset. The plan warns the user to stop the old writer. A small heartbeat object in S3 would turn silent corruption into a startup error. This is a suggestion, not a demonstrated defect.
- `CONTEXT.md` says the daemon "preserves the upstream SMB server's Time Machine features" in the present tense while README says planning only. Wording only.
- The credential refresh section in the plan and issue 17 correctly separates S3 access credentials from TLS certificate replacement. I found no contradiction there.
- The AGPL-3.0-only choice is consistent with the copied AGPL go-smb2 server and the Apache-2.0 JuiceFS source as long as notices and the source offer remain. The MPL-2.0 lru dependency is also compatible. No action.

## Speculative concerns, not demonstrated

These are not findings. The plan or the research notes say the evidence is missing.
- Time Machine's behavior after an outer-filesystem metadata rollback. Nothing has tested it.
- Export memory, duration and write contention for very large namespaces. The source loads all metadata tables in memory and no measurements exist.
- S3 listing consistency during remote-state classification on the eventual provider. No provider is chosen.
- Availability of a passphrase helper on a bare recovery machine. Helpers may depend on a login session.
- Concurrent writers on two machines. The plan forbids them and does not claim to detect them.

## Bottom line

The plan is close, and its settled choices are coherent. The passphrase question has a real answer: the RSA key cannot be derived from the passphrase, but a passphrase-protected key stored in the bucket makes the passphrase the only user-retained secret. That option needs a security decision and a small, native-compatible implementation path. Before final approval, resolve questions 1 through 5 and apply the corrections in findings 3, 5, 6 and 8. Findings 1, 4 and 7 need user decisions. Finding 2 needs a recorded platform check before any port or privilege decision.

## Genuine user questions and recommended answers

1. What encryption recovery material should the first release require?

Question: Can recovery need only a passphrase as the encryption secret, with no separate key file?

Recommended: The passphrase cannot replace the RSA key. It only unlocks the PEM. A passphrase can be the only secret you personally keep if a passphrase-protected copy of the PEM is stored in the bucket and fetched during recovery. Recommend keeping the external PEM copy as the default and adding this bucket-stored mode as an explicit option. If you choose it, use a PKCS#8 PBES2-encrypted PEM produced with the bundled gmsm library instead of the legacy format, require a long random passphrase, store the object outside `meta/` and `chunks/`, and accept that anyone who can read the bucket holds the encrypted key for offline guessing and anyone who can write the bucket can destroy recovery. The sentence "A password alone cannot restore the dataset" is technically true but misleading and should be rewritten.

2. Should S3 credentials refresh while the daemon runs?

Recommended: Resolve at startup and keep the values until restart for the first release. This question is about S3 access keys and optional session tokens. It is not about TLS certificates. Ordinary access keys stay valid until replaced or revoked. Temporary session credentials expire, and the startup-only policy must not pretend otherwise. If you plan to use temporary credentials, decide that now and ask for a refresh or re-read policy before implementation.

3. What does the target platform actually permit on port 445?

Recommended: Keep the 445 default for now and record a platform check before any decision. The check should state the target OS and configuration, whether the unprivileged daemon binds 445, the exact failure if it does not, and whether a standard client connects. Treat both the privileged-port rule and the Time Machine port requirement as unverified assumptions until that record exists. If the bind fails, bring the result back and decide between a different port, elevated execution or a forwarding step. Do not adopt a root-run policy on an assumption.

4. Should every writable start create a fresh metadata export?

Recommended: No. Require a successful backup inside a defined age window and gate native cleanup on the newest successful snapshot. Keep the immediate stop after a failed scheduled backup. This keeps the recovery promise without turning every restart into an export or refusing to start during a short S3 problem.

5. Is a correctness-first release with no performance target acceptable?

Recommended: Add the small measured SMB check now. The first real workload is the Mac and Time Machine, and a performance failure found there is expensive to fix after the first release.
