# s3-smb plan audit: encryption and recovery

Reviewed revision `fa36915f6a0de6fdb12b1292cc2102a79349071d`. Workspace HEAD matches it, and the reviewed documents have no local changes.

Actual environment:

```text
PI_MODEL=gpt-6-astra
PI_PROVIDER=openai-codex
PI_REASONING_LEVEL=xhigh
```

I read the complete implementation plan, README, CONTEXT, issue snapshot and all four supplied research notes. This is a plan audit, not an application-code review. I inspected dependency source without modifying it. I ran no builds or crypto/recovery tests. Prior probe results below are the research authors' recorded results, not tests rerun here.

## Answer to the passphrase question

Yes. The user can retain only a passphrase as the encryption secret if S3 retains a separately passphrase-encrypted copy of the randomly generated JuiceFS RSA private key. This is technically feasible from the inspected source, but it is not yet an approved or tested product design.

Native JuiceFS still needs the actual RSA private key. Its passphrase unlocks serialized key bytes; it does not reconstruct the RSA key. Native object encryption generates random symmetric keys and wraps them with RSA-OAEP. The encrypted metadata backup cannot bootstrap its own decryption, even though its contents include the stored private-key field. See K, lines 47-140 and 226-282, and M, lines 5031-5052.

The smallest source-compatible option needs no new encryption protocol or additional crypto dependency:

1. Generate a random RSA key once, after the user confirms initialization.
2. Encode it as a standard encrypted PKCS#8 `ENCRYPTED PRIVATE KEY` PEM using the already selected `github.com/emmansun/gmsm` library, with explicit password-KDF and authenticated-cipher options.
3. Store those encrypted bytes at one documented, stable S3 object name derived from the configured volume prefix. Keep it outside `meta/` and `chunks/`. Use the underlying S3 store, not JuiceFS's RSA-encrypted object wrapper, or recovery becomes circular.
4. On a fresh installation, fetch that object using separately supplied S3 access credentials, unlock it with the passphrase, then decrypt and import the native metadata backup. A separately retained private-key file is unnecessary.

The inspected library supports PBES2 with scrypt and AES-256-GCM, and JuiceFS's parser calls that library for encrypted PKCS#8. This establishes source support, not an executed compatibility test. Native key parsing, wrong-passphrase rejection and the complete S3 recovery sequence still need acceptance tests.

The user still needs S3 access and the nonsecret connection/volume settings. Optional mutual TLS also requires a usable client certificate and its private key, or a way to obtain replacements. Neither is the JuiceFS encryption secret. After loss of local state, recovery fails if the passphrase or the only surviving key object is lost. Anyone who obtains the encrypted key object can attempt offline password guessing, so recommend a long, randomly generated passphrase.

## Material findings

Five findings follow. Findings 1, 3 and 5 contain demonstrated source behavior. Findings 2 and 4 identify missing plan rules. Their failure scenarios are reasoned consequences, not reproduced application failures.

### 1. High: "native-compatible PEM" does not specify safe password protection

Evidence: P, lines 57 and 84-88, proposes a passphrase-protected PEM and reuse of native-compatible encoding. K, lines 47-63, implements `ExportRsaPrivateKeyToPem` with deprecated `x509.EncryptPEMBlock`. That is legacy PEM encryption, not encrypted PKCS#8. X, lines 79-105 and 186-214, shows fast MD5-based key expansion and unauthenticated CBC. Merely switching to the selected PKCS#8 library's defaults is insufficient: G2, lines 85-97, selects AES-256-CBC and PBKDF2-HMAC-SHA256 with only 2,048 iterations.

Failure scenario: An implementation follows the native exporter or library defaults and uploads the resulting key file to S3. A bucket reader can attack a human passphrase cheaply. AES-256-GCM for data objects does not improve the password protection of the RSA key. Legacy/CBC key encryption also lacks cryptographic ciphertext authentication. I did not test a password-cracking or padding-oracle attack.

Smallest correction: Name an explicit authenticated PKCS#8 profile and forbid the legacy exporter and implicit library defaults for newly generated keys. The existing library supports scrypt plus AES-256-GCM. A concrete candidate is a 16-byte random salt, scrypt `N=131072, r=8, p=1`, a 12-byte random GCM nonce and a 16-byte tag. The scrypt working allocation is about 128 MiB. Measure startup cost on the supported platform before fixing the profile; do not silently reduce its cost. Keep the random RSA key independent of the passphrase. Pass the passphrase directly to the parser rather than exporting it through process arguments or `JFS_RSA_PASSPHRASE`.

User decision needed: No decision about cryptographic internals. The user must choose the retention model in finding 2.

### 2. High, conditional on the S3-held key option: bootstrap durability is not specified

Evidence: P, lines 84-88, correctly marks the external-file workflow as unapproved, but provides no replacement S3-key lifecycle. Startup rules at lines 103-109 and acceptance at line 159 still lack that branch. Issue 17 in the snapshot explicitly requests this review. K, lines 351-360, encrypts every `Put` through the wrapped store, so uploading the bootstrap key through it would require the missing RSA key to retrieve that key.

Failure scenario: Initialization acknowledges a working dataset before its recovery key is safely retrievable. The machine is then lost. Alternatively, a retry replaces the only key object, cleanup removes it, or an existing key object with no metadata backup is mistaken for a new dataset. Native encrypted backups cannot repair any of these cases.

Smallest correction: If selected, add the stable raw-S3 key location and creation/recovery order to issues 21, 24, 25 and 27. Create it without overwriting an existing object. Verify a downloaded copy unlocks to the generated key before accepting writable service, and still require the initial metadata backup. Treat ambiguous upload results and key-only remote state as partial initialization, never as permission to generate a replacement key. Keep this object out of native backup rotation and lifecycle expiry. Validate the decrypted backup's volume identity before publishing local metadata. Missing, corrupt or mismatched key material must stop recovery without remote mutation.

Update the fresh-install test to delete every local key as well as state/cache/config, then recover using only the passphrase, S3 access and documented settings. Include interrupted key publication and missing/overwritten key-object cases. No new recovery archive or key-management service is needed.

User decision needed: Yes. Approve or reject S3-held encrypted key recovery. Do not keep the separate-PEM requirement as a universal statement if this option is chosen. The current plan explicitly labels it a previous proposal, so this is an unresolved design gap, not a claim that the user already approved it.

### 3. Medium, conditional on fetching a key from S3: parsing needs limits before password derivation

Evidence: P specifies output limits for credential helpers at lines 76-78, but no corresponding fetched-key limits. K, lines 67-113, accepts several key formats and can fall back to unencrypted key parsing despite a supplied passphrase. G1, lines 49-87, and G2, lines 170-193, decode KDF parameters and derive the key before returning. G3, lines 24-33, forwards scrypt parameters directly. C, lines 195-208, checks mathematical limits but allocates `128*N*r` bytes without an application memory budget.

Failure scenario: A small malformed or replaced key object requests an enormous valid scrypt cost and exhausts memory before authentication. A permissive importer may also accept an unencrypted replacement when the selected workflow promises password-protected key storage. These paths were inspected, not executed.

Smallest correction: Bound the downloaded object and decoded structure. Before invoking the KDF, allow only the selected encrypted PKCS#8 profile, supported cost parameters, nonce/tag sizes and RSA key sizes. For this auto-generated format, accepting one known profile is simpler than accepting arbitrary algorithms. Reject plaintext and legacy-PEM fallback on the S3 bootstrap path. Return sanitized errors for wrong passphrase, corruption and unsupported format. Test excessive cost parameters without actually allocating the requested memory, plus truncation, modified ciphertext and unsupported key types.

User decision needed: No. These checks enforce the selected format without creating a new one.

### 4. Medium: recovery overrides credentials but does not establish transport-setting precedence

Evidence: P, line 108, says current credentials override an older export. It does not say the same for endpoint, region or TLS settings. P, lines 55-56, otherwise requires explicit transport settings and certificate verification. F, lines 236-307, reconstructs storage from the saved format, interprets `tls-insecure-skip-verify` in its bucket URL, and reads TLS file paths from that URL. Its native CA setup requires all three TLS files and replaces system roots. S, lines 490-624, derives region/path-style behavior partly from URL/environment settings rather than the proposed YAML fields.

Failure scenario: Following native reconstruction literally applies new credentials to an old destination or restores old HTTP/TLS settings. A private-CA-only configuration may also fail if implementation copies the CLI TLS helper. These are integration risks, not evidence that the proposed application already leaks credentials. The plan rightly avoids the CLI entry point, but should specify what replaces its configuration behavior.

Smallest correction: State that current validated YAML owns S3 destination, region, path style and TLS for bootstrap, normal startup and recovery. Treat saved format locations as consistency information, not permission to redirect current credentials or load local certificate paths. A conflict should stop or require an explicit documented destination change. Wire one verified transport before any S3 request, add private roots to system roots, and keep client authentication independent of custom-CA use. Add a recovery test with stale endpoint/TLS fields in the export. No certificate hot-reload product is needed.

User decision needed: No. This makes the existing connection and verification promises executable.

### 5. High: the reused backup helper can report success after gzip finalization fails

Evidence: P, lines 109, 132-134 and 159-160, requires a successful recoverable backup and fail-stop behavior on staging errors. B, lines 118-140, calls `_ = zw.Close()` and proceeds to size/copy the staged file if `DumpMeta` succeeded. The gzip writer can still have buffered bytes and its trailer to write during `Close`.

Failure scenario: Staging fills during final compression output or trailer writing. Export returned nil; gzip close fails; the helper ignores that failure and uploads a truncated stream. S3 can accept and encrypt those bytes successfully, after which startup or cleanup treats an unusable backup as protection. This is an unchecked-error path demonstrated by source, not a fault test run in this audit.

Smallest correction: Explicitly include gzip finalization and staging error propagation in issue 24's result-returning native backup extraction. Do not upload, rotate or advance protection state unless finalization succeeds. Add a regression that allows export writes but fails the final gzip writes and asserts no successful replacement. This is a narrow native correctness correction, not a new backup format.

User decision needed: No.

## Credentials, TLS and exposure conclusions

S3 credential refresh means obtaining a new access-key/secret-key/session-token set while the daemon is running. It is unrelated to replacing TLS certificates. The pinned S3 constructor uses `NewStaticCredentialsProvider`; that provider has no expiry metadata or file/command refresh. S3 can still reject those credentials after expiry or revocation. See S, lines 609-624, and A, lines 21-61.

A TLS certificate authenticates the network peer. Private CA settings affect trust; a mutual-TLS client certificate/key authenticates this client. Renewing a server certificate under an already trusted CA normally requires no credential-helper refresh. With the proposed startup-loaded TLS files, replacing a local CA or client-certificate file takes effect after restart. Document that behavior rather than adding another unresolved rotation product.

The existing helper and logging rules are sensible: direct argv execution, no interactive stdin, bounded output/runtime, no fallback, private output capture and synthetic-secret checks across both log streams. Keep secrets out of argv and environment when wiring the native parser. Suppressing application logs cannot conceal secrets that a user explicitly puts in process arguments.

Remote encryption does not encrypt the local SQLite database, WAL, cached file data or plaintext metadata staging. Q, lines 208-296, derives format-field wrapping from the stored UUID; that is not protection against someone who can read local state. Keep the planned private directories and staging cleanup, and say this plainly in recovery/security documentation. Do not imply that the passphrase protects a compromised running daemon.

The plan correctly keeps AGPL-3.0-only settled, allows native writable recovery, distinguishes local-backend probes from S3 tests, and requires real public-install and platform evidence. I found no basis here for a password-derived RSA scheme, allocator redesign, mandatory second dataset or mandatory external private-key file. I did not expand the review into speculative storage attacks or additional product requirements.

## Evidence index

Line references above use these exact inspected files. JuiceFS source is the supplied v1.4.1 snapshot at `0b90c7db5a929ae6adc5faad948d108efd2c99f9`; the bundled copy relocates imports. GMSM is the selected `v0.41.1` dependency, not an assumed future library.

- P: `/home/joshazimullah.linux/work_mounts/s3-time-machine/docs/implementation-plan.md`
- K: `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/object/encrypt.go`
- M: `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/meta/sql.go`
- Q: `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/meta/config.go`
- B: `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/vfs/backup.go`
- S: `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/object/s3.go`
- F: `/tmp/s3-time-machine-recovery-research/.research/sources/juicefs/cmd/format.go`
- G1: `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/emmansun/gmsm@v0.41.1/pkcs8/pkcs8.go`
- G2: `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/emmansun/gmsm@v0.41.1/pkcs/pkcs5_pbes2.go`
- G3: `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/emmansun/gmsm@v0.41.1/pkcs/kdf_scrypt.go`
- GCM support: `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/emmansun/gmsm@v0.41.1/pkcs/cipher.go`, lines 173-250; `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/emmansun/gmsm@v0.41.1/pkcs/cipher_aes.go`, lines 88-97.
- C: `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/golang.org/x/crypto@v0.49.0/scrypt/scrypt.go`
- A: `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/aws/aws-sdk-go-v2/credentials@v1.19.14/static_provider.go`
- X: `/usr/local/go/src/crypto/x509/pem_decrypt.go`. This installed source is Go 1.25.6, as recorded in `/usr/local/go/VERSION`; it is not the research probes' Go 1.26.3 toolchain.

Other complete inputs:

- `/home/joshazimullah.linux/work_mounts/s3-time-machine/README.md`
- `/home/joshazimullah.linux/work_mounts/s3-time-machine/CONTEXT.md`
- `/tmp/s3-smb-plan-audit/issues.json`, including issue 18 and audit ticket 30, selected by their `number` fields.
- `/tmp/s3-time-machine-bundled-research/docs/research/bundled-source-install.md`
- `/tmp/s3-time-machine-recovery-research/docs/research/metadata-backup-and-recovery.md`
- `/tmp/s3-time-machine-writable-research/docs/research/writable-recovery.md`
- `/tmp/s3-time-machine-logging-research/docs/research/embedded-logging.md`

The recorded recovery probes retained an external key and used a local object backend. They do not prove the new S3-held-key bootstrap. The writable-recovery note supersedes the earlier allocator concern. PKCS#8/GCM interoperability and KDF cost here remain source-inspected proposals, not measured results or claims about every OpenSSL version.

## Genuine user questions and recommended answers

1. Do you want S3 to retain the passphrase-encrypted JuiceFS key, so you need not separately retain a private-key file? Recommended answer: yes, if the dependency on preserving that S3 object is acceptable. Keep only the passphrase as the separately retained encryption secret, plus S3 access and connection details. An offline encrypted-key copy can remain optional. This choice is still pending.

2. Is restarting after S3 access-credential replacement acceptable, or must expiring credentials refresh without interruption? Recommended answer: startup-only resolution for the first release if you use long-lived keys and accept restarts. If you intend to use short-lived session credentials for longer-running service, specify refresh now, including refreshing the complete credential set together. This question is not about TLS certificate renewal.
