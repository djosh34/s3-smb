# Every reviewer comment and its disposition

This is the complete comment index for the four final plan reviews. It includes alternatives, minor observations, cautions and disagreements, not only the recommendations chosen for the plan. Repeated questions are mapped to their findings rather than counted twice.

The reports reviewed revision `fa36915f6a0de6fdb12b1292cc2102a79349071d`, before the user made encryption optional. They are preserved as written and are not the current specification. The [implementation contract](../implementation-plan.md) is the approved specification, with execution still on hold; [the unified audit](plan-audit.md) groups overlapping findings. Subsequent user updates require passwordless SMB, decimal units and zero-cache tests, permissions-as-warnings, local Docker-first testing and a final hosted-Mac Time Machine test. These later changes were not audited by the four original reports.

## Full reports

- [Astra storage, extra-high](raw/astra-storage.md)
- [Astra crypto, extra-high](raw/astra-crypto.md)
- [DeepSeek contract, max](raw/deepseek-contract.md)
- [DeepSeek readiness, max](raw/deepseek-readiness.md)

"Included" below means added to planning requirements or tests, not implemented or experimentally proved. "Not selected" means the recommendation remains visible here but is not a current requirement. The user has approved the resulting plan and explicitly deferred execution; these dispositions remain visible for traceability.

## Recommendations not adopted as written

1. **External-key recovery as the default, plus an optional S3-key mode.** DeepSeek contract recommended both, and moving the local key outside the state directory or verifying an external copy. The user selected the S3-held protected key instead. A separate PEM is not mandatory. An independent copy remains optional; it is not another application mode. Optional encryption now means encrypted versus unencrypted datasets.
2. **An S3 heartbeat to prevent two writers.** DeepSeek contract suggested this as a smaller observation. It might detect some accidental overlap, but a heartbeat alone cannot prevent a stale writer continuing to write. The plan retains the stated single-writer restriction and local state lock. A real cross-machine lease/fencing design is not selected.
3. **A data scan before calling recovery complete.** DeepSeek readiness proposed data checks, with sampling as a cheaper option. The concern is included, but neither a mandatory full scan nor a new scrub command is selected. A sample does not prove every object exists; an existence check alone does not prove every object decrypts. The plan must label metadata restoration accurately, surface missing-data errors and fully verify the disposable acceptance fixture.
4. **A supported inode/chunk maximum or a native binary-export fallback.** DeepSeek readiness offered these responses to fast JSON export memory use. Measurements and safe failure behavior are included. A hard cap or alternate format is deferred until measurements justify it. A binary path would need its own consistency/encryption/load tests; it is not rejected as technically impossible.
5. **A prescribed root-run route or changing the default port.** Early DeepSeek feedback treated low-port binding as universally root-only. That premise was corrected. The current XNU check distinguishes specific addresses from wildcard binds; Linux policy is configurable. The plan keeps the explicit bind choices, records platform tests and documents permission failures. It does not silently widen the bind, install privilege machinery or require a new user decision based on the original blanket claim.
6. **Reject, warn about or remove short-lived session credentials.** DeepSeek readiness suggested this alongside startup-only loading. The user has permanent keys and confirmed startup-only loading. No renewal workflow or expiry-policy feature is needed. A manually supplied optional token remains static; the app does not claim to renew it or infer an opaque token's expiry.
7. **A mandatory read-only rehearsal before the Mac interruption test.** DeepSeek contract suggested a rehearsal in a throwaway state directory. It is a useful optional precaution and is recorded in the release checklist, not a mandatory recovery mode. The actual interruption/recovery test must use disposable data and stop the old writer.
8. **A new performance target or a separate correctness-first product decision.** DeepSeek contract asked for measurements and raised whether a correctness-first release was acceptable. Measurements are included. No unsupported latency target, sizing questionnaire or extra release mode was added. Linux remains first; the user now requires hosted-Mac Time Machine validation as the final implementation task.
9. **Fail whenever the native UUID marker is missing.** DeepSeek contract proposed stopping if data or backups exist without it. The no-overwrite concern is included, but a valid backup can supply the native UUID during confirmed recovery. The absence of a redundant marker must not itself make otherwise recoverable data inaccessible. Unknown or conflicting state still fails safely.
10. **Different backup naming and age-policy implementations.** Astra storage offered unique names with a retention-parser change as an alternative to collision checks. The plan chose unused native timestamps with bounded waiting/failure. DeepSeek contract suggested a broad retention-minus-margin startup window; the plan chose a stricter recent-point reuse rule and explicit operation/deletion deadlines. Both underlying concerns are included. These alternatives were not silently treated as defects.

## Astra storage

| Original finding or comment | Disposition |
| --- | --- |
| 1. Native file flush does not establish the promised SQLite synchronization. | Included. Select FULL on every connection rather than an unspecified equivalent sync operation. Check pragmas and error propagation; SIGKILL is not a power-loss test. |
| 2. Gzip close errors can be ignored and a truncated backup uploaded. | Included. Check finalization and test a failure during the final writes, then restore the unchanged prior point. |
| 3. Root native contexts permit client trash purges. | Included. Use an unprivileged client identity and protect resolved trash entries, including aliases and handles. |
| 4. Cleanup needs an enforcement point inside native work. | Included. Check before reference retirement and deletion, including queued work and suspension races. Validate effective native retention. |
| 5. Resource-fork/xattr writes discard backend errors. | Included. Propagate failure and test read-only/injected-failure wire responses. |
| 6. Native close methods and loops do not supply bounded shutdown. | Included. Define mutable-worker shutdown, close the SQL engine separately and enforce a hard process deadline. Harmless process-lifetime loops need not all be rewritten. |
| 7. Second-resolution backup names can overwrite the prior object. | Included. Do not reuse a colliding or ambiguously uploaded name. The unique-suffix alternative was not selected. Bucket versioning is not required. |
| Passphrase-only retained encryption secret through a protected key in S3. | Included and subsequently selected by the user for encrypted datasets. Publish/verify the key before service; keep it outside native cleanup. |
| No basis to reopen native writable recovery or redesign allocation counters. | Included as an evidence limit. The earlier same-dataset probe remains narrow; real S3 tests still need to run. |
| Two final questions about credential refresh and key location. | Answered. Startup-only permanent credentials; S3-held protected key when encryption is enabled. |

## Astra crypto

| Original finding or comment | Disposition |
| --- | --- |
| 1. Native-compatible PEM and library defaults do not specify safe password protection. | Included. Specify authenticated PKCS#8, explicit KDF/cipher parameters and measurements. Pass the secret directly in memory, not through application-set argv/environment. |
| 2. The remote key's publication, identity and failure lifetime are unspecified. | Included. Fixed raw-S3 location, no overwrite, readback/unlock verification, partial-initialization handling and cold recovery without an external PEM. |
| 3. A fetched key can request excessive KDF work or exploit permissive format fallback. | Included. Bound input, validate the selected profile before derivation and reject plaintext/legacy fallback on encrypted-key recovery. |
| 4. Restored transport settings could override current YAML. | Included. Current destination/region/path-style/TLS are authoritative; reject conflicts and do not follow old paths or insecure options. |
| 5. The backup helper ignores gzip finalization errors. | Included with the overlapping Astra storage finding. |
| Certificate replacement is distinct from S3 credential renewal. | Included. Local TLS files load at startup; normal server-certificate renewal is not an S3 credential change. |
| Existing direct-argv, bounded-output and secret-safe logging requirements are sensible. | Retained. No additional logging framework or credential manager was requested. |
| Remote encryption does not protect local SQLite/WAL/cache/staging or a compromised running process. | Included in security documentation. Remote metadata backups are encrypted when enabled. The user confirmed that these remote copies were the concern, so supported native local SQLite/WAL/staging remain unchanged; no local encryption product is added. |
| No evidence for password-derived RSA, mandatory external PEM, allocator redesign or a second dataset. | Retained as limits on scope. |
| Two final questions about key location and credential renewal. | Answered by the user. Their answers do not prove the unimplemented encrypted-key round trip; its tests remain required. |

## DeepSeek contract

| Original finding or comment | Disposition |
| --- | --- |
| 1. Passphrase-only recovery is a key-location choice, with offline-guessing risks. | Included. The external-key-default/two-mode recommendation was not selected; the user chose the S3-held key for encrypted datasets. |
| 2. Port 445 needs platform evidence and a usable documented path. | Included as a platform check and clear bind/config-path diagnostics. A universal root requirement was corrected, not adopted. |
| 3. Remote-state classification lacks marker/key/backup rules. | Included. Partial states are nonempty. Unlike the proposed blanket stop, a validated backup can recover a missing marker's UUID. |
| 4. A fresh export at every ordinary restart is unnecessarily strict; the age gate is vague. | Included. Reuse a verified recent point without resetting its schedule, otherwise make a new one. Define and test deletion deadlines. |
| 5. Shared temporary staging and native secret wrapping are not adequate local protection. | Included with a later user change: create private staging/files, but warn about existing owner/mode issues rather than reject readable files. Keep cleanup and the explicit distinction between remote encryption and local plaintext. |
| 6. The Mac test must restore an interrupted backup, then resume and restore new data. | Included. The suggested read-only rehearsal is optional, not a new product restriction. |
| 7. Browsing and export performance have no measured evidence. | Included. Record basic measurements. No new performance promise or extra correctness-first approval question was added. |
| 8. Existing insecure secret files have no exact accept/reject rule. | Superseded in part by the user's explicit request. Existing secret-file ownership/0400/0600 checks now warn, not reject. New files are private by default; public certificates are not secret files. |
| Smaller observation: the install fixture kept SMB external. | Included. The plan now narrows that proof and tests the bundled SMB closure separately. |
| Smaller observation: use an S3 heartbeat to detect another writer. | Not selected. It remains visible above; a heartbeat is not a fencing mechanism. |
| Smaller observation: CONTEXT described the unimplemented daemon in present tense. | Corrected to describe the planned daemon. |
| Smaller observation: AGPL-3.0-only appears compatible with the selected dependency grants. | Recorded as no identified blocker. Full notices, source offer and license review remain release work. |

The contract reviewer also listed five explicitly unverified concerns. None is silently converted into a demonstrated bug:

- Time Machine behavior after metadata rollback remains unproven. The user now requires automated hosted-Mac full-backup/crash/restore acceptance as the final task, not a later manual-user test.
- Large-namespace export memory, duration and write contention remain measurement requirements.
- The eventual S3 provider's behavior still needs evidence. A fixture is not a claim about every provider.
- Credential/passphrase helper programs may be unavailable or logged out on a fresh machine. Recovery documentation must explain that they are user-supplied and that literal/file sources are available; it must not assume the old helper session survives.
- Concurrent independent writers remain unsupported and not reliably detected across machines.

Its five proposed user questions map to these rows. Credential loading and key location are now answered. Startup reuse and performance measurements were incorporated into the draft. The unsupported universal port-privilege premise was corrected rather than put to the user as a forced root-run choice.

## DeepSeek readiness

| Original finding or comment | Disposition |
| --- | --- |
| 1. Passphrase-only recovery still needs the actual native key somewhere. | Included. S3 retains the protected key for encrypted mode; the user retains the passphrase. Newly selected unencrypted mode needs neither. |
| 2. Credential refresh differs from certificate rotation; tokens can expire. | Included as clarification. Startup-only is confirmed. The proposed token ban/warning/renewal feature was not selected. |
| 3. Metadata import does not prove all referenced data survives. | Included. Accurate completion messages, missing-object failures and full fixture verification. Mandatory production scans were not selected; sampling is not complete proof. |
| 4. Pin a safe key-protection format instead of legacy PEM. | Included with the Astra crypto finding. |
| 5. The adapter must own write-through behavior. | Included. Implement flush-before-success; do not use the suggested unsupported-operation fallback instead of the required behavior. |
| 6. Fast consistent export has an unmeasured memory cost. | Measurements and safe failure are included. A hard supported-count cap or binary fallback is deferred pending evidence. |
| 7. Correct statuses need server changes, not only adapter errors. | Included. Keep specific mappings and wire tests rather than weaken the requirement to any failed status. |
| 8. The Darwin privilege rule distinguishes specific from wildcard addresses. | Included after source checking. Keep the default, test actual behavior and never broaden binding automatically. |
| Minor observation: trash days are a native format setting. | Included. Apply through native configuration, read back and validate the effective retention. |
| Minor observation: custom CA roots belong in the process TLS pool, not OS trust. | Wording and requirements corrected. |
| Minor observation: retries lack count/delay/deadline rules. | Included. The contract specifies attempts, delays and a shared total deadline. |
| Ready areas: install direction, license, logging, native cache, one-writer policy, XDG and source forms. | Retained. These are planning conclusions, not completed application acceptance tests. |
| Final credential/key questions. | Answered by the user; no certificate-rotation question remains. |

## Corrections and later user decisions

The initial legacy-PEM description said "unsalted". Source checking showed that the MD5-based expansion uses salt; it is still fast and the CBC format is unauthenticated. The reviewers corrected that assertion. Their final reports are preserved here, and the earlier correction is recorded rather than hidden.

One report described loss of S3 credentials as unrecoverable. That is too broad: valid replacement credentials for the same dataset can work. The original access-key bytes are not required. Losing the encryption passphrase or all copies of the random encryption key is a different problem.

The initial broad low-port/root claim was also corrected. The exact target Mac still needs its runtime bind/client test. The application does not acquire privileges or silently switch to wildcard binding.

After the reviews, the user confirmed permanent credentials and startup-only resolution. They selected the S3-held protected key and explicitly made encryption optional, accepting disclosure of data and metadata to anyone with sufficient S3 read access when disabled. The updated plan adds both-mode tests. Do not claim the four earlier reviews already tested or audited that later change.

[Approve the implementation-ready s3-smb backlog](https://github.com/djosh34/s3-smb/issues/29) records the user's approval and explicit instruction not to execute yet. Application work awaits a later start instruction.
