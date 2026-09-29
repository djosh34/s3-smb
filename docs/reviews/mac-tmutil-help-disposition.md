# First native run: narrow `tmutil` documentation-gate correction

> **Historical failure/disposition.** Preserve this evidence; its review/release
> sequence and old full-source gates are not current instructions. The
> [user-approved Mac replacement](../macos-acceptance.md) governs subsequent work.

Failed immutable run: [36584525709](https://github.com/djosh34/s3-smb/actions/runs/36584525709)
at `cb49ec8360a397658542ea042533c1d7b990a169` / public `v0.1.0-rc.2`.

**Native application build/public install and matching native MinIO build passed.**
The final acceptance then **failed**, before service/source/SMB/Time Machine work,
because the harness required literal `smb` in `tmutil help setdestination`.
The exact exit0 help lists `mount_point` and an AFP URL. The same delivered
manual explicitly documents an SMB destination and `protocol://user[:pass]@host/share`.
This is a directly demonstrated documentation-inference defect, not a platform
permission/provider block or demonstrated unsupported SMB capability.

## Diagnosis and smallest correction

The failure stack, retained exit0 help and real-assertion-path RED reproducer
identify the literal-check cause directly. Per the task's narrow authorization,
a speculative multi-hypothesis investigation was unnecessary. No attack testing,
permission changes, additional Mac probe or retry was used for diagnosis.

- Preserve per-verb help output/status as diagnostic evidence; do not interpret
  its abbreviated prose as an exhaustive capability/options list.
- Require the installed manual's **correct verb section** to document the options
  actually used. For setdestination it must explicitly establish SMB plus the
  supported URL form. Missing documentation still fails.
- Check the same inference boundary for equivalent assumptions: grouped short
  flags and abbreviated summaries must not hide documented long flags/`-X`.
  An option documented for another verb cannot satisfy this verb's requirement.
- The actual recorded read-only `status` command succeeded although absent from
  the manual. Require that actual operation and parsed status at the platform
  gate rather than an unobserved help summary. Subsequent real status gates stay.

The actual named-empty `smb://timemachine:@127.0.0.1/TimeMachine` destination
operation is **unchanged**. No AFP, guest, mount-point reinterpretation, fixture
backup, source shrinking or local-snapshot restore fallback was added. Full
baseline/resumed pre-wipe expectations, actual completed backups, native points,
wipes/default recovery, full native restores, metadata comparison and real
interruption/remote-read assertions are unchanged. Only their documentation
preflight was corrected. No production/module/shared-Linux/fixture change.

## Reproduction and preservation

`test/macos/platform-fixtures/` contains the exact captured setdestination help
and status outputs plus byte-identical selected manual lines, with source/hash
provenance. The original complete artifact remains public on the rc2 release.

`test_tmutil_contract.py` drives **Acceptance.platform() itself**, stopping only
when the next independent check is reached. It was run before the correction:
`/tmp/s3-smb-swarm/mac-tmutil-contract-red.log` records the same literal assertion
failure. After correction `mac-tmutil-contract-green.log` records six passing
checks: real-path captured regression; equivalent brief-help omissions; required
SMB/URL contract absence; option scoping/grouped flags; missing verb/help failure;
and unchanged named-empty SMB destination operation. Synthetic other-verb help
summaries are regression inputs, not claimed native observations.

Original failed workflow, logs, help/manual and inventory remain unchanged.
All25 inventoried entries plus inventory matched after download; actual uploader
verification and upload passed. Issue34 was inadvertently auto-closed by GitHub's
interpretation of a negative completion sentence on PR36 merge; it was reopened,
the PR wording corrected, and the timeline preserved. Use only plain issue
references/acceptance-pending wording and verify states after future merges.

## Still held

This correction requires a new immutable snapshot and fresh narrow verification,
then complete local Linux release, identical same-SHA CI and a new same-SHA public
candidate/fresh install before another final Mac run. No native retry is yet
authorized. Issues34/18 remain OPEN and compatibility remains unclaimed.

Only disk free space was observed in the failed run; normally eligible source
size/capacity planning were not reached. No platform-impossibility or capacity-
deficit inference follows from that run.
