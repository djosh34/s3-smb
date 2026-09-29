# s3-smb

A foreground SMB server backed by S3 through embedded JuiceFS and local SQLite metadata. No FUSE mount, separate metadata server, or complete local data replica is required.

**Development status:** implementation is underway; release acceptance is not complete. Do not use this development version for irreplaceable backups. In particular, real hosted-Mac Time Machine full backup and crash recovery remain required before a compatibility claim.

## Install and run

The release installation interface is:

```sh
go install github.com/djosh34/s3-smb@<version>
s3-smb version
s3-smb serve -c /path/to/config.yaml
```

A published, validated release version will replace `<version>` in release notes. Go 1.26.3 and a normal C compiler/platform SDK are required. SQLite and compression CGo dependencies use bundled portable source; no separately installed third-party native libraries are required. Public versioned installation is a release gate, not established by a local checkout build.

Start with the [complete YAML example and settings reference](docs/configuration.md). The default config path is `$XDG_CONFIG_HOME/s3-smb/config.yaml`, otherwise `$HOME/.config/s3-smb/config.yaml`, on Linux and macOS. `-c` and `--log-format text|json` work before or after `serve`.

`serve` stays in the foreground. There is no separate `init`, daemonization, service installer, backup scheduler, or custom file browser. Help and version do not initialize storage or resolve credentials.

### First use

Use a dedicated, existing S3 bucket and independently save the configuration details and recovery secrets. Run from a controlling terminal: the application asks before initializing a genuinely empty dataset or recovering existing remote metadata. Unknown objects, missing markers or a missing local database are not proof of an empty dataset. Normal restarts do not require a terminal.

The default share is `TimeMachine`, listening on `127.0.0.1:445`. Access it using your SMB client and configured named account. Do not infer Time Machine compatibility from the share name or from a successful file copy. Binding the configured address may require platform-specific privileges; the application does not silently widen the address or change ports.

## Storage and access

- One SMB share/account, with a password or explicit named-empty access (`password: ""`). Omitting the password is an error. Empty-password access uses ordinary named-account authentication/signing, not anonymous guest mode.
- Wider binding such as `0.0.0.0:445` is explicit. Combining it with empty-password access exposes the share to anyone who can reach it and produces a warning. Optional read-only serving is available.
- Native JuiceFS data caching. Public sizes are decimal: **1 MB = 1,000,000 bytes; 1 GB = 1,000,000,000 bytes**. Omitted capacity retains the native default; explicit `cache_size: 0` disables retained disk/RAM block caches, not SQLite, temporary staging or working I/O buffers.
- Independent S3 access-key and secret-key sources: literal value, file, or direct command argv. Resolve once at startup, with no implicit shell, fallback or automatic renewal. Helpers run with the daemon's privileges; trust the config.
- Custom S3 endpoints, explicit path-style or virtual-host-style addressing, verified HTTPS, private CA roots and mutual TLS. Local certificate replacement takes effect after restart. Intentional HTTP must be configured explicitly.
- Application encryption defaults on. Data and entire remote metadata exports use the native encryption key; its passphrase-protected copy is retained in S3. Fresh-install recovery needs S3 access, connection details and the passphrase, not an independently retained PEM file.
- Explicit `encryption.enabled: false` opts out for both data and remote metadata. Anyone with sufficient S3 read access can then read them. TLS, SMB access policy and metadata protection still apply. Dataset encryption mode cannot silently change.
- Local SQLite/WAL, caches and temporary export staging remain plaintext. New state/secret files are created privately; unusual existing permissions/ownership produce warnings rather than rejection when files remain readable.

## Recovery and protection

Read [the recovery procedure and operating boundaries](docs/recovery.md) before storing important data.

The default is an hourly native metadata export and 14-day native trash retention, both configurable. These metadata backups describe the outer filesystem; they are not Time Machine backups. Losing local state can lose changes after the selected successful metadata point.

A scheduled backup that fails after bounded retries stops writable service. A metadata import alone does not prove all referenced file objects exist. Verify recovered contents, and use Apple's actual restore tools for a Time Machine dataset.

**Only one writable metadata authority may use a dataset.** The local lock cannot fence a different host with a different SQLite database. Stop the old writer before recovery. External S3 lifecycle deletion can destroy keys, data or recovery points despite application retention.

## Development and evidence

```sh
scripts/test-linux.sh suite
```

The shared Docker entry point builds the application and a pinned MinIO fixture, runs unit/race tests and actual SMB-to-S3 integration tests, and retains logs/artifacts. GitHub Linux CI must run this same entry point after local success. The separate `release` mode also checks the required coverage ledger; a green targeted test is not release approval. See [testing](docs/testing.md), [logging](docs/logging.md) and [source packaging](docs/packaging.md).

The final [hosted-Mac gate](docs/macos-acceptance.md) uses a normal full-Mac Time Machine backup, then transfers the stopped MinIO store to a second fresh Mac for application recovery and Apple's native restore. Verification covers only deliberately created files and folders, including nested and empty directories, against an independent reference. Normal recovery must pass before later crash/resume acceptance. No fixture-only backup, generic copy, local-snapshot restore or metadata import substitutes for it. Harness-only iterations reuse the qualified application version and record harness/application revisions separately.

### Project references

- [Approved implementation contract](docs/implementation-plan.md) and [delivery backlog #18](https://github.com/djosh34/s3-smb/issues/18).
- [Domain terminology](CONTEXT.md), [decision map](https://github.com/djosh34/s3-smb/issues/1), and [scope clarification](https://github.com/djosh34/s3-smb/issues/33).
- [Plan approval](https://github.com/djosh34/s3-smb/issues/29), [four-reviewer audit](docs/reviews/plan-audit.md), and [all original review comments](docs/reviews/reviewer-comments.md).
- [Passwordless/zero-cache research](docs/research/guest-and-zero-cache.md) and [hosted-Mac prerequisites](docs/research/github-macos-time-machine.md), with their original evidence limits.

The user released the prior execution hold on 2026-09-29. Authorization and source inspection are not completed acceptance evidence.

## License and source

Original code is **AGPL-3.0-only**. Bundled upstream code retains its original licenses and attribution; see [LICENSE](LICENSE), [NOTICE](NOTICE) and [packaging provenance](docs/packaging.md). Corresponding source and build material are public at [github.com/djosh34/s3-smb](https://github.com/djosh34/s3-smb); use the tag/commit matching the distributed version. Redistributors of modifications must provide their own corresponding source, not merely link to this unmodified repository.
