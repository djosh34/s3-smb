# s3-smb

A terminal-only Go SMB server that stores files in S3-compatible storage through embedded JuiceFS and its supported SQLite metadata backend.

**Status: plan approved; execution on hold at the user's request. Research probes have run, but the application has not been implemented or tested end to end. Do not use this project for real backups yet.**

## First-release direction

- Planned installation: `go install github.com/djosh34/s3-smb@<version>`, without a checkout; no GUI.
- Run `serve` in the foreground. The user manages the process; no launchd/systemd integration, service installer, or daemonization.
- No separate `init` command. When the configured dataset is genuinely new, `serve` asks for `y/n` confirmation before creating it. Missing local SQLite alone must not trigger a new dataset over existing remote data.
- Use one YAML config at `$XDG_CONFIG_HOME/s3-smb/config.yaml`, or `~/.config/s3-smb/config.yaml` when unset. Accept `-c <path>`.
- Each S3 access key and secret key independently accepts a literal value, file, or directly executed argument array whose stdout supplies the credential. No implicit shell. Support custom S3 endpoints, explicit path-style addressing and TLS certificates.
- Use standard `log/slog`, with text and JSON-only log-output modes. Keep interactive prompts and secrets out of the JSON log stream.
- Portable bundled C/CGo code is allowed. Installation may need a normal C compiler and platform SDK, but no separately installed third-party native libraries.
- Focused `macos-fuse-t/go-smb2` fork and direct in-process JuiceFS adapter; no FUSE dependency or mount.
- Reuse JuiceFS's native caching, including tested cache size `0`. Use decimal MB/GB for settings and display. Support remote datasets larger than the daemon's available local storage.
- Default to `127.0.0.1`. Support passwordless SMB as well as account/password access, and allow explicit `0.0.0.0` binding. Passwordless means a named account with an explicitly empty password; no anonymous mode is required.
- Create private local files by default, but warn rather than reject existing secret files solely because of ownership or permission modes such as 0400/0600.
- Linux/ARM64 development first. Run almost all tests locally on the VM in Docker with MinIO and the application. GitHub Linux CI repeats the exact same suite as extra verification, not the primary debugging loop.
- Actual Time Machine full backup, normal recovery and crash-during-write recovery on GitHub-hosted macOS is the final required task, after everything else passes. Back up the Mac runner's normally eligible contents, not just a controlled test dataset. Hosted-runner runtime capability is still unverified, not established as impossible.
- Access files through SMB. No custom browser or migration tool.
- Offer optional JuiceFS encryption, on by default. When enabled, keep the passphrase-protected key in S3; no separately retained key file is required. Explicit `encryption.enabled: false` means anyone with sufficient S3 read access can read the data and metadata. TLS, S3 authentication and the selected SMB access policy still apply.
- Encrypt remote metadata backups in encryption-enabled mode with the same native key used for data. The user confirmed that only the remote backups were the concern; live local SQLite/WAL and staging remain native and unencrypted.
- Back up metadata to S3 hourly and retain deleted/replaced data through JuiceFS trash for 14 days by default. Both settings are configurable and apply in either encryption mode.
- Stop with a clear error if a scheduled metadata backup fails after normal retries. Preserve the previous usable backup; do not silently keep running without protection.
- If local metadata is missing but the remote dataset exists, `serve` offers confirmed recovery through JuiceFS's native restore path, then resumes normal writes to the same dataset. Also support an optional read-only mode. Verify the recovery path before claiming compatibility.
- Recover on a fresh installation using S3 and externally saved secrets, without any files from the old machine. Losing changes since the last successful metadata backup is acceptable. Prefer JuiceFS's native periodic backups over per-write remote metadata synchronization.
- No separate backup-management product, Time Machine scheduler, or custom backup-history browser.

## Planning

- [Wayfinding map](https://github.com/djosh34/s3-smb/issues/1): decision index.
- [Implementation backlog](https://github.com/djosh34/s3-smb/issues/18): eleven implementation sub-issues with native dependencies, acceptance tests and review requirements.
- [Implementation contract](docs/implementation-plan.md): required behavior, native integration work, necessary fixes and the local-first acceptance sequence. Follow JuiceFS for ordinary implementation details.
- [Passwordless and zero-cache research](docs/research/guest-and-zero-cache.md): named-empty NTLM passed a small probe; cache-zero behavior is source-verified, not yet application-tested.
- [Hosted-Mac Time Machine prerequisites](docs/research/github-macos-time-machine.md): service/permission and storage constraints; no Mac CI run has happened. The earlier fixture-only recommendation was rejected.
- [Confirmed scope clarification](https://github.com/djosh34/s3-smb/issues/33): named-empty SMB password, remote-only encryption concern and full Mac backup/restore scope.
- [Confirmed credential, encryption and license choices](https://github.com/djosh34/s3-smb/issues/17): permanent credentials loaded at startup, optional encryption with an S3-held protected key when enabled, and AGPL-3.0-only.
- [Unified four-reviewer audit](docs/reviews/plan-audit.md): findings from two Astra extra-high and two DeepSeek V4.1 Flash max reviews.
- [Every reviewer comment](docs/reviews/reviewer-comments.md): complete summaries, disagreements, dispositions and the four final reports.
- [Final plan approval](https://github.com/djosh34/s3-smb/issues/29): the user approved the plan and explicitly deferred execution.

The backlog is approved. Its `status:awaiting-execution` label records the user's hold, not an unresolved design question. Do not start until the user explicitly authorizes implementation.

### Starting implementation later

In this repository, give a coding agent this request; no special slash command is required:

```text
Start implementing the approved backlog:
https://github.com/djosh34/s3-smb/issues/18

This authorizes execution and releases the previous hold. Follow
README.md, CONTEXT.md, docs/implementation-plan.md and the native issue
dependencies. Meet each task's tests and review requirements before
closing it. Run the shared Docker/MinIO tests locally before the same
suite in GitHub CI. Keep full Mac Time Machine backup/crash/restore
acceptance last. Do not reduce the agreed scope or silently skip failures.
```

That is a future invocation example, not authorization from this document.

The destination is an agreed implementation backlog with parent issues, sub-issues, dependencies, acceptance tests, and correctness/security review requirements. Specify **what to build**, not how autonomous agents coordinate. Avoid speculative infrastructure.

Keep pinned JuiceFS and customized dependency source in ordinary packages in this repository. Preserve their behavior and adapt imports for packaging. Record source revisions and retain licenses/notices. Do not rewrite SQL behavior merely to avoid module replacements.

Do not submit or request upstream JuiceFS fixes, maintain a collection of separate dependency forks, or make our release wait for upstream changes. The chosen source layout passed a clean Linux/ARM64 module-proxy installation probe. Installation of the actual public application remains a release test.

The [handoff](docs/smb-s3-time-machine-handoff.md) preserves research and earlier proposals with the current scope noted at the top. Unconfirmed recommendations are not requirements, and source inspection is not compatibility evidence.

AGPL-3.0-only is the confirmed license for original project code. Full license text, dependency notices and source-distribution information remain implementation requirements.
