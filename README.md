# s3-smb

A terminal-only Go SMB server that stores files in S3-compatible storage through embedded JuiceFS and its supported SQLite metadata backend.

**Status: planning only. Research probes have run, but the application has not been implemented or tested end to end. Do not use this project for real backups yet.**

## First-release direction

- Planned installation: `go install github.com/djosh34/s3-smb@<version>`, without a checkout; no GUI.
- Run `serve` in the foreground. The user manages the process; no launchd/systemd integration, service installer, or daemonization.
- No separate `init` command. When the configured dataset is genuinely new, `serve` asks for `y/n` confirmation before creating it. Missing local SQLite alone must not trigger a new dataset over existing remote data.
- Use one YAML config at `$XDG_CONFIG_HOME/s3-smb/config.yaml`, or `~/.config/s3-smb/config.yaml` when unset. Accept `-c <path>`.
- Each S3 access key and secret key independently accepts a literal value, file, or directly executed argument array whose stdout supplies the credential. No implicit shell. Support custom S3 endpoints and TLS certificates.
- Use standard `log/slog`, with text and JSON-only log-output modes. Keep interactive prompts and secrets out of the JSON log stream.
- Portable bundled C/CGo code is allowed. Installation may need a normal C compiler and platform SDK, but no separately installed third-party native libraries.
- Focused `macos-fuse-t/go-smb2` fork and direct in-process JuiceFS adapter; no FUSE dependency or mount.
- Reuse JuiceFS's native caching. Explicitly support remote datasets larger than the daemon's available local storage.
- Default to `127.0.0.1` with simple authentication. Allow explicit `0.0.0.0` binding.
- Linux/ARM64 development and testing first. Preserve go-smb2's Time Machine-related SMB features. The user will test with a Mac later.
- Access files through SMB. No custom browser or migration tool.
- Use JuiceFS encryption. Back up metadata to S3 hourly and retain deleted/replaced data through JuiceFS trash for 14 days by default. Both settings are configurable.
- Stop with a clear error if a scheduled metadata backup fails after normal retries. Preserve the previous usable backup; do not silently keep running without protection.
- If local metadata is missing but the remote dataset exists, `serve` offers confirmed recovery through JuiceFS's native restore path, then resumes normal writes to the same dataset. Also support an optional read-only mode. Verify the recovery path before claiming compatibility.
- Recover on a fresh installation using S3 and externally saved secrets, without any files from the old machine. Losing changes since the last successful metadata backup is acceptable. Prefer JuiceFS's native periodic backups over per-write remote metadata synchronization.
- No separate backup-management product, Time Machine scheduler, or custom backup-history browser.

## Planning

- [Wayfinding map](https://github.com/djosh34/s3-smb/issues/1): decision index.
- [Implementation backlog](https://github.com/djosh34/s3-smb/issues/18): ten implementation sub-issues with native dependencies, acceptance tests and review requirements.
- [Implementation contract](docs/implementation-plan.md): detailed behavior and release acceptance matrix.
- [Remaining choices](https://github.com/djosh34/s3-smb/issues/17): credential refresh and initial encryption-key handling. AGPL-3.0-only is confirmed.
- [Unified four-reviewer audit](docs/reviews/plan-audit.md): findings from two Astra extra-high and two DeepSeek V4.1 Flash max reviews, with accepted corrections and the remaining questions.
- [Final plan approval](https://github.com/djosh34/s3-smb/issues/29): blocks all implementation until shared understanding is confirmed.

The backlog is a complete draft, not an approved execution plan. Use wayfinder, grilling, domain-modeling, and unslop. Update issue bodies after each round.

The destination is an agreed implementation backlog with parent issues, sub-issues, dependencies, acceptance tests, and correctness/security review requirements. Specify **what to build**, not how autonomous agents coordinate. Avoid speculative infrastructure.

Keep pinned JuiceFS and customized dependency source in ordinary packages in this repository. Preserve their behavior and adapt imports for packaging. Record source revisions and retain licenses/notices. Do not rewrite SQL behavior merely to avoid module replacements.

Do not submit or request upstream JuiceFS fixes, maintain a collection of separate dependency forks, or make our release wait for upstream changes. The chosen source layout passed a clean Linux/ARM64 module-proxy installation probe. Installation of the actual public application remains a release test.

The [handoff](docs/smb-s3-time-machine-handoff.md) preserves research and earlier proposals with the current scope noted at the top. Unconfirmed recommendations are not requirements, and source inspection is not compatibility evidence.

AGPL-3.0-only is the confirmed license for original project code. Full license text, dependency notices and source-distribution information remain implementation requirements.
