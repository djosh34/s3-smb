# Recovery and operating boundaries

Release acceptance is still in progress. Do not use this development version for irreplaceable backups. See the [implementation contract](implementation-plan.md) for the release gates.

## What must survive a lost machine

Keep the following outside the daemon's local state and cache:

- The S3 bucket, region, endpoint and addressing/TLS configuration, plus the volume identity.
- Working S3 access credentials. Replacement credentials may be used if they authorize the same dataset.
- The encryption passphrase, when encryption is enabled.
- Any private-CA roots or mutual-TLS credentials needed to connect.

In encrypted mode the bucket also contains a passphrase-protected native private key at `s3-smb/keys/<volume-UUID>.pem`. The passphrase does not reconstruct a missing key. Losing that object and all independent copies makes encrypted data unrecoverable. An independent copy is optional, not a prerequisite for fresh-install recovery.

The nonsecret connection/format identity is at `s3-smb/format.json`. Native data, metadata exports and the UUID marker use the `s3-smb/` volume prefix; the key and identity are outside native chunk/backup cleanup. The local active database is `metadata.db` inside `storage.state_dir`.

The daemon's metadata backups contain the filesystem namespace, attributes and block mappings. They are not copies of the live SQLite file and are not Time Machine backups. File objects and the entire remote metadata export are encrypted in encrypted mode. Local SQLite/WAL, caches and temporary export staging remain plaintext with private creation permissions. Use an appropriately trusted local machine.

## Recover without the old local files

1. Stop the old writer. Do not start another writer against this dataset while it is still running.
2. Install the same tested release and recreate the configuration from the independently saved connection details and secrets. Use a new local state directory; the old cache is not required.
3. Run `s3-smb serve -c /path/to/config.yaml` in a terminal. Missing local metadata must lead to remote inspection, not automatic formatting.
4. Read the selected metadata recovery point and the warning about losing changes after it. Confirm recovery only if this is the intended dataset and the old writer has stopped.
5. The application must decrypt/decompress and load that point into a fresh temporary SQLite database, validate it, and establish a new successful metadata backup before writable service begins.
6. Read and verify the recovered contents through SMB. A successful metadata import does not prove that every referenced data object exists. Missing data must be an explicit read error, not an empty-file success.
7. Resume writes only after checking the expected files. For a Time Machine dataset, use Apple's Time Machine restore and backup tools as well; listing sparsebundle files over SMB does not prove the inner backup can be restored.

Do not delete remote objects, reformat the bucket, change encryption mode, or generate a replacement key to get past a recovery error. A corrupt selected backup must fail visibly rather than silently choose an older point. Keep logs and preserve the bucket for diagnosis.

Normal restarts with valid local metadata do not require a terminal. New initialization and recovery do: confirmation uses the controlling terminal, not the log stream. A missing terminal is an error when consent is needed.

## Protection and retention

Defaults are an hourly native metadata backup and 14-day native trash retention. A scheduled backup that cannot complete after bounded retries stops writable serving. An operation that is stuck or overdue is not a successful backup. Preserve the previous successful point while investigating.

An old metadata backup does **not** guarantee its referenced blocks are still retained. Do not configure external S3 lifecycle rules to delete live data, protected keys, identity objects or recovery points based only on object age. Bucket lifecycle policies can defeat application retention and make recovery impossible.

The local state lock prevents concurrent use of the same local metadata authority. It is not a distributed lease: another host with a different SQLite database is not fenced. Never run two independent writable metadata authorities for one dataset.

The required SMB FLUSH/write-through durability is the application's data flush and FULL-mode SQLite synchronization; executed acceptance evidence must establish this before release. It does not synchronously create a new remote metadata backup for every write. Recovery after loss of local state can lose changes since the selected successful export. Process-kill tests are not proof of survival through power loss.

## Access and secrets

The default SMB listener is `127.0.0.1:445`. Explicit wider binding is supported but exposes access to other hosts. Named-empty passwordless access still names an account and uses native NTLM/signing; it is not anonymous guest access. Missing password configuration must not silently enable it.

With `encryption.enabled: false`, S3 readers with sufficient access can read data and metadata. TLS, S3 credentials, the configured SMB access policy and metadata protection still apply. Encryption mode is fixed when a dataset is created; changing the YAML is not a conversion procedure.

Permanent S3 credentials and the enabled encryption passphrase are resolved once at startup. Helpers execute direct argv without an implicit shell, with the daemon's privileges; that is not a sandbox. Restart to load changed secrets or TLS files. Readable pre-existing secret/state files with unusual permissions or ownership cause warnings, not automatic refusal. New files are created privately.

## Evidence limits

Linux SMB-to-S3 tests, native source inspection, and a Darwin build are separate evidence. None alone establishes Time Machine compatibility. The release requires the final hosted-Mac full backup, fresh-state recovery, observed crash during a later backup with S3 writes in flight, restore of completed data, resumed backup and another restore. Until those pass, no production Time Machine claim is made.
