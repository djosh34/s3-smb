# Captured native CLI contract fixtures

Source: first actual final Mac run
https://github.com/djosh34/s3-smb/actions/runs/36584525709 at
`cb49ec8360a397658542ea042533c1d7b990a169`, macOS15.7.9/24G830,
Darwin ARM64. Full original artifacts are retained in the failed-run bundle on
https://github.com/djosh34/s3-smb/releases/tag/v0.1.0-rc.2 .

- `macos-15.7.9-setdestination-help.txt`: **entire exact stdout** of
  `/usr/bin/tmutil help setdestination`, exit0, artifact `0002-tmutil.log`.
  SHA256 `7758f0367f1d027e38a4ea676be750175cd27618082cf7252629e4fe7338c25c`.
- `macos-15.7.9-status.txt`: **entire exact stdout** of the bounded diagnostic
  `/usr/bin/tmutil status`, exit0, artifact `0003-tmutil.log`.
- `macos-15.7.9-man-contract-excerpt.txt`: selected **unchanged lines**, not the
  whole manual, from artifact `0001-sh.log` (`man tmutil | col -b`, exit0).
  Retains the relevant verb signatures plus the destination's explicit SMB and
  URL-form documentation. Full original manual SHA256
  `ecadcaf6ebe7c21cab9343b18a8efb715a632e495d647055b899a3f92ffe8252`.
  Every selected line was checked byte-for-byte against that original output.

The brief help omits SMB despite the delivered manual explicitly documenting it.
The original platform assertion fails on this exact help; the corrected path
requires the full per-verb manual contract and actual operations, not exhaustive
wording in brief help. Tests deliberately abbreviate *other* help output to
exercise the same inference boundary; those synthetic summaries are not claimed
as observed platform output. Grouped short flags and documented long options
remain checked in the correct verb's manual section.

The recorded status operation succeeded but that verb is absent from the manual.
The platform gate therefore requires the real read-only status command and its
parsed result, not an invented help listing. No SMB mount, destination selection,
backup or restore occurred in this first run. These fixtures do not prove any of
those capabilities and never replace their required real acceptance operations.
