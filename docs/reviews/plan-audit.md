# Unified implementation-plan audit

Four reviewers audited plan revision `fa36915f6a0de6fdb12b1292cc2102a79349071d` and its GitHub issue bodies. All ran through Pi. Their configured models and reasoning levels were checked through Paseo and reported from their session environments.

| Reviewer | Model | Reasoning | Main focus |
| --- | --- | --- | --- |
| Astra storage | `openai-codex/gpt-6-astra` | `xhigh` | Durability, SMB, backup and cleanup |
| Astra crypto | `openai-codex/gpt-6-astra` | `xhigh` | Passphrase recovery, cryptography, credentials and TLS |
| DeepSeek contract | `openrouter/deepseek/deepseek-v4.1-flash` | `max` | Requirements, contradictions and unnecessary restrictions |
| DeepSeek readiness | `openrouter/deepseek/deepseek-v4.1-flash` | `max` | Execution details and acceptance evidence |

The reviewers inspected source and earlier research. They did not implement the application or run new storage/crypto probes. Their reports were checked, deduplicated and corrected where necessary. The findings below are the consolidated result, not four separate lists of requirements.

The [implementation contract](../implementation-plan.md) and [implementation backlog](https://github.com/djosh34/s3-smb/issues/18) include the accepted technical corrections and have now been approved by the user. Execution is explicitly on hold until a later start instruction. The user has since confirmed startup-only permanent credentials, the S3-held protected key when encryption is enabled, and optional application encryption. AGPL-3.0-only is approved. Optional encryption was added after these reviews; do not attribute that later change to the reviewed revision.

[Every reviewer comment and its disposition](reviewer-comments.md) includes alternatives, minor observations, cautions and links to all four complete final reports. Comments not selected as requirements are preserved there, not silently omitted.

Later user changes also require SMB access without a password (withdrawn on 2026-10-02), decimal MB/GB, tested native zero cache, warnings for existing file-permission issues, local Docker/MinIO-first tests and a required hosted-Mac Time Machine test as the final task. The contract now specifies outcomes and necessary native integration/fixes rather than prescribing ordinary JuiceFS mechanics or illustrative internal constants. These changes postdate the four reviews.

## Direct answers

### S3 credentials are not TLS certificates

The refresh question concerns the access key, secret key and optional session token that authorize S3 requests. Ordinary keys usually remain valid until someone replaces or revokes them. Temporary session credentials expire. A long-running daemon using those credentials needs renewal before expiry, or a restart with new credentials.

TLS certificates serve a different purpose. A renewed server certificate under an already trusted CA normally needs no change to the daemon's S3 credentials. Under the proposed startup-only TLS loading, replacing a local CA file or mutual-TLS client certificate/key requires a restart. No automatic certificate-rotation system is proposed.

### You can retain only a passphrase as the encryption secret

JuiceFS still needs a random private key internally. The application can store that key in S3 in a standard passphrase-encrypted format, fetch it on a fresh installation and unlock it with the passphrase. You then do not need to retain a separate private-key file.

This is source-supported, not yet tested end to end. S3 access credentials and the endpoint/bucket/volume details remain necessary. Optional mutual TLS has separate client credentials too.

The encrypted key must remain available in S3. Losing it and all optional copies makes recovery impossible even with the passphrase. Anyone who can obtain it can attempt offline password guessing. Use a long random passphrase and modern password protection. The native legacy PEM exporter is not suitable for this new bootstrap design.

## Consolidated findings

### 1. High: specify safe key protection and its S3 lifetime

The native PEM exporter uses deprecated, unauthenticated legacy encryption with fast MD5-based password expansion. The selected PKCS#8 library also has weak defaults for this purpose: CBC and 2,048 PBKDF2 iterations. Merely saying "use native encryption" does not choose safe protection for a cloud-held private key. [S1][S2]

For the now-selected S3-held key in encrypted mode, the plan requires authenticated encrypted PKCS#8 through the compatible dependency, with explicitly reviewed password-derivation parameters rather than weak defaults. The earlier scrypt/AES-GCM profile remains a source-supported candidate; implementation must pin and test the chosen profile. It also specifies a bounded key object, parameter validation before costly password derivation, one stable bootstrap location outside native data/backup cleanup, upload/readback verification, and no replacement key on failed recovery. A missing or damaged key must not trigger initialization.

The key-location choice is now confirmed. Native-parser and fresh-install tests are still required. Unencrypted mode must bypass key generation, fetching and passphrase resolution entirely. It does not need a new encryption protocol, password-derived RSA, or key-management service.

Work belongs in [encrypted storage](https://github.com/djosh34/s3-smb/issues/21), [metadata recovery](https://github.com/djosh34/s3-smb/issues/24), [serve startup](https://github.com/djosh34/s3-smb/issues/25) and [integration tests](https://github.com/djosh34/s3-smb/issues/27).

### 2. High: successful SMB flush needs durable local metadata

Native file Fsync flushes the data writer, but the selected SQLite driver defaults to WAL with synchronous NORMAL. That is weaker than the plan's local metadata synchronization promise. Killing a process and reopening the database does not simulate a host power failure. [S3]

The plan now requires `_synchronous=FULL` on every SQLite connection, effective-pragma checks and synchronization-error propagation. This does not add per-write S3 metadata backups.

Work belongs in [encrypted storage](https://github.com/djosh34/s3-smb/issues/21) and [the SMB adapter](https://github.com/djosh34/s3-smb/issues/22). No user decision is needed.

### 3. High: SMB can acknowledge writes that the backend rejected

The known FLUSH bug is not the only discarded error. The resource-fork/xattr WRITE path ignores `Setxattr` failure and reports all bytes written. The server also passes write-through flags to the adapter without implementing that durability itself. Some generic error mappings cannot produce the statuses promised by the plan. [S4]

The plan now names these patch sites, makes the adapter honor write-through, and specifies error classes/statuses for wire-level tests. Read-only or failed attribute writes must not report success.

Work belongs in [the SMB adapter](https://github.com/djosh34/s3-smb/issues/22) and [authenticated SMB and error handling](https://github.com/djosh34/s3-smb/issues/23). No user decision is needed.

### 4. High: a native backup can report success after gzip finalization fails

The backup helper ignores `gzip.Close` errors. JSON export can succeed while the final compressed bytes or trailer fail to reach staging. Uploading that file can produce a completed S3 object that cannot be restored. [S5]

The result-returning backup operation must check finalization and staging errors before upload, success recording or rotation. The plan now requires a failure injected specifically during gzip finalization and a cold restore of the preserved earlier point.

Work belongs in [metadata protection](https://github.com/djosh34/s3-smb/issues/24). No user decision is needed.

### 5. High: native cleanup and client trash purges can bypass protection

A fresh backup alone does not stop an SMB client using a root native context from purging trash. Nor does `NoBGJob` stop all native deletion work: workers start separately, and reads can trigger compaction. After a long suspension, cleanup may resume before the backup timer. [S6]

The plan now requires an unprivileged client identity, protection of the resolved trash namespace, and a live check before native reference retirement and object deletion, including queued work. Implementation must define and test a bounded protection/deletion policy against actual operation and shutdown budgets and effective retention. The earlier illustrative constants/formula are not a new product requirement. A test must release cleanup before the delayed backup callback and prove the protected point still restores.

Work belongs in [storage](https://github.com/djosh34/s3-smb/issues/21), [the adapter](https://github.com/djosh34/s3-smb/issues/22) and [metadata protection](https://github.com/djosh34/s3-smb/issues/24). No user decision is needed.

### 6. Medium: backup names can collide and replace the prior point

Native backup names contain only whole seconds. Rapid restarts, short intervals or clock changes can reuse a name. A PUT that succeeds remotely but loses its response can then overwrite the prior object while the daemon reports failure. [S5]

The plan now prohibits reusing an existing or ambiguously uploaded name. Wait for an unused native timestamp within the operation deadline or fail safely. Keep the real snapshot time for age calculations. Test collisions, backward clocks and lost responses. Bucket versioning is not a new requirement.

Work belongs in [metadata protection](https://github.com/djosh34/s3-smb/issues/24). No user decision is needed.

### 7. Medium: bounded shutdown is not provided by native Close calls

Native flush/close paths can wait longer than the proposed shutdown bound, some loops live for the process lifetime, and closing a metadata session is distinct from closing the SQL engine. A wrapper timeout alone can leave writers alive after their resources or state lock are released. [S7]

The plan now defines the shutdown order and a hard process-exit deadline. Mutable workers need cancellation/join treatment; harmless process-lifetime loops may end with the process. It does not require rewriting every native loop into a reusable library. Tests must stall real native I/O in a subprocess.

Work belongs in [storage](https://github.com/djosh34/s3-smb/issues/21) and [foreground serving](https://github.com/djosh34/s3-smb/issues/25). No user decision is needed.

### 8. Medium: remote-state classification and restored connection settings need rules

The native identity marker lives in CLI initialization, not in the embedded constructor, and its write failure is only a warning there. Marker-only, key-only and data-without-backup states need explicit treatment. Restored format settings must not redirect current credentials or weaken current TLS policy. [S8]

The plan now defines those states, treats partial state as nonempty, verifies identity and leaves unknown objects untouched. Current validated YAML controls destination, region, addressing and TLS. Old export settings are consistency information, not permission to load old certificate paths or contact another endpoint.

Work belongs in [storage](https://github.com/djosh34/s3-smb/issues/21), [recovery](https://github.com/djosh34/s3-smb/issues/24) and [startup](https://github.com/djosh34/s3-smb/issues/25). No user decision is needed.

### 9. Medium: the plan overstated what startup and recovery checks prove

Requiring a full new export on every ordinary restart was stricter than the agreed protection rule. Conversely, calling a successfully imported dump "usable" could imply that every referenced file had been checked. Metadata can load even when a referenced object is missing. Sampling is not proof that all data survives.

An ordinary restart may now reuse a confirmed recent successful point for the same volume, without resetting its original schedule. Initialization and recovery still require a new point before writable serving. Missing or stale success evidence requires a backup, not reliance on the native attempt marker.

Recovery output must distinguish metadata import from full content verification. Missing data must produce explicit errors, not empty-file success. Integration fixtures verify every expected file and include missing-object cases. No mandatory full-dataset download or new scrub product is added.

Work belongs in [metadata protection](https://github.com/djosh34/s3-smb/issues/24), [startup](https://github.com/djosh34/s3-smb/issues/25) and [integration tests](https://github.com/djosh34/s3-smb/issues/27). No user decision is needed.

### 10. Medium: private staging and local plaintext need explicit treatment

Native backup staging uses the system temp directory and default file permissions. Remote encryption does not encrypt local SQLite/WAL, cached contents or temporary exports. Native format-field wrapping is not local-disk encryption. [S5][S9]

The latest user requirement keeps private creation defaults and safe abandoned-file cleanup, but changes existing secret-file ownership/permission checks to warnings, not startup rejection. Actual staging/read/write failures remain errors. Public CA/certificate files are not private keys. Credential helpers run with the daemon's privileges, so config trust still matters even without a shell.

Work belongs in [configuration](https://github.com/djosh34/s3-smb/issues/20), [storage](https://github.com/djosh34/s3-smb/issues/21), [metadata protection](https://github.com/djosh34/s3-smb/issues/24) and [release documentation](https://github.com/djosh34/s3-smb/issues/28). No user decision is needed.

### 11. Medium: export resources and browsing performance were not measured

The consistent fast SQLite export materializes metadata in memory. The earlier research explicitly left its memory, duration and write contention to be measured. Cache correctness alone also says nothing about browsing latency. [S9]

The plan now asks for peak memory/staging/duration evidence and simple SMB listing, cold/warm read and write measurements. It does not invent a dataset-size cap, fixed throughput promise, replacement cache or another sizing questionnaire. Resource failure must preserve the prior point and never silently select inconsistent export.

Work belongs in [metadata protection](https://github.com/djosh34/s3-smb/issues/24) and [integration tests](https://github.com/djosh34/s3-smb/issues/27). No user decision is needed.

### 12. Medium: the Mac test must include interrupted-backup recovery

Mounting, reconnecting and reading files do not establish that an outer-filesystem rollback leaves Time Machine usable. The now-required final hosted-Mac test must complete a baseline backup, interrupt a later backup during S3 writes, discard local daemon state, recover through the normal policy, restore older files, resume backup and restore from a new backup. It also requires normal recovery from a full backup of the Mac runner's normally eligible contents. The user rejected reducing the Mac backup to a controlled fixture. It must use disposable data with the old writer stopped.

One reviewer initially claimed all low-port binds require root on macOS/Linux. That was too broad. Current XNU source checks reserved-port privilege for a specific address, including loopback, but treats wildcard binding differently. Linux policy is also configurable. This is source evidence, not a Mac runtime test. Keep the loopback default, record actual bind results and never silently switch to a wildcard address. No new root-run policy or privilege-management feature is approved. [S10]

Work now belongs in [Last: prove Time Machine full backup and crash recovery on GitHub macOS](https://github.com/djosh34/s3-smb/issues/34), blocked by every earlier implementation task. Hosted-runner prerequisites are researched but untested. Full normal Mac backup scope is confirmed; a fixture-only, generic SMB or manual-user substitute is not authorized. Disabled-by-default Time Machine is not proof of an unsupported runner.

### 13. Low: narrow the installation proof to what actually ran

The packaging fixture bundled JuiceFS and its customized dependencies, but imported SMB as an external module. Copying the SMB source for local protocol fixes still needs validation. The report must not describe the future complete application as already installed successfully.

The contract now says exactly what passed. Both the bundled SMB layout and the actual public module remain acceptance checks in [source packaging](https://github.com/djosh34/s3-smb/issues/19) and [release validation](https://github.com/djosh34/s3-smb/issues/28).

## Alternatives, disagreements and minor comments

The [complete comment index](reviewer-comments.md) records every main finding, smaller observation, speculative concern and repeated question. It identifies recommendations included in the plan, adopted with changes, deferred pending evidence or not selected. It also records corrected claims rather than presenting them as facts.

The recommendations not adopted as written include an external-key-default/two-mode design, an S3 heartbeat, mandatory recovery scans, hard namespace limits or binary fallback before measurement, a prescribed root-run route, token expiry features, a mandatory read-only rehearsal, another performance-target decision, blanket refusal when a UUID marker is missing, and alternative naming/freshness policies. Their authors, reasons and current treatment are summarized in that index.

## User answers after review

1. Use the user's permanent S3 credentials and resolve them at startup. No automatic refresh or temporary-credential workflow is required.
2. When encryption is enabled, store the protected key in S3 so the passphrase is the only separately retained encryption secret. S3 access and connection details are still required.
3. Encryption is optional. With explicit `encryption.enabled: false`, data and metadata have no application encryption. The user accepts disclosure to anyone with sufficient S3 read access. TLS, authentication, backups and protection rules remain active. Both-mode acceptance tests have been added; the earlier reviews did not test this new choice.
4. AGPL-3.0-only is confirmed.

The user has given [final plan approval](https://github.com/djosh34/s3-smb/issues/29) and explicitly said not to execute yet. The approved requirements do not constitute completed implementation or test evidence.

## Sources and evidence limits

[S1]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/object/encrypt.go#L47-L140
[S2]: https://github.com/emmansun/gmsm/blob/v0.41.1/pkcs/pkcs5_pbes2.go
[S3]: https://github.com/mattn/go-sqlite3/blob/v1.14.24/sqlite3.go
[S4]: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/server/file_tree.go#L617-L862
[S5]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/backup.go
[S6]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/meta/base.go
[S7]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/writer.go#L386-L512
[S8]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/cmd/format.go
[S9]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/meta/sql.go#L4808-L5052
[S10]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet/in_pcb.c#L987-L1003

1. Native key export, parser, RSA wrapping and encrypted object handling: [JuiceFS encryption][S1]. Standard password-protection options: [GMSM PBES2][S2], [PKCS#8](https://github.com/emmansun/gmsm/blob/v0.41.1/pkcs8/pkcs8.go), [scrypt](https://github.com/emmansun/gmsm/blob/v0.41.1/pkcs/kdf_scrypt.go), [AES-GCM](https://github.com/emmansun/gmsm/blob/v0.41.1/pkcs/cipher_aes.go). Legacy PEM deprecation and salt use were checked in the installed Go 1.26.3 source at `crypto/x509/pem_decrypt.go`.
2. SQLite WAL defaults: [driver][S3], [JuiceFS connection setup](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/meta/sql.go#L448-L457), and [native file flush/fsync](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/fs/fs.go#L1440-L1490).
3. Error acknowledgements: [SMB handlers][S4]. Backup finalization/names/staging: [native backup helper][S5]. Maintenance/trash: [native metadata lifetime][S6]. Shutdown waits: [native writer][S7].
4. Format creation, identity and transport wiring: [native command code][S8]. Snapshot memory and secret fields: [SQL export][S9].
5. Darwin bind distinction: [tagged XNU IPv4 check][S10]. Linux configurable policy: [kernel IP sysctl documentation](https://docs.kernel.org/networking/ip-sysctl.html#ip-unprivileged-port-start).

These source findings establish code behavior and identify failure cases to test. They do not establish that the unfinished application passes those tests, that the proposed PKCS#8 profile has been benchmarked, or that real Time Machine recovery works. The earlier native writable-recovery and packaging probes retain their original, narrower evidence limits.
