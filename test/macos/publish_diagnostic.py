#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Publish fixed-field producers only. Native/app/system logs stay encrypted."""
import json
from pathlib import Path
import re
import shutil
import sys

TIMESTAMP = re.compile(r'"time"\s*:\s*"([0-9T:.+Z-]{10,40})"')


def publish(private, public):
    private, public = Path(private), Path(public)
    public.mkdir(parents=True, exist_ok=False)
    # Audited producers emit build identity or fixed classifications/numbers.
    # Client parser preserves unknown/collection failures; no raw-log fallback.
    for name in ('harness-revision', 'application-version', 'application-source-revision',
                 'application-tag-object', 'application-sha256.txt', 'native-build.txt',
                 'application-variant.txt', 'minio-revision', 'capture-command.json',
                 'framing-error-counts.json', 'measurement.json', 'platform.txt',
                 'workload-outcome.json', 'cleanup-outcome.json', 'client-outcome.json',
                 'status-samples.jsonl', 'geometry-samples.jsonl', 'resources.jsonl', 'resource-health.json',
                 'budget-preflight.json', 'store-samples.jsonl'):
        path = private / name
        if path.is_file():
            shutil.copyfile(path, public / name)
    for name in ('ready.json', 'summary.json', 'samples.jsonl'):
        path = private / 'capture' / name
        if path.is_file():
            (public / 'capture').mkdir(exist_ok=True)
            shutil.copyfile(path, public / 'capture' / name)
    path = private / 'commands.jsonl'
    if path.exists():
        with path.open() as source, (public / 'command-timings.jsonl').open('w') as target:
            for line in source:
                row = json.loads(line)
                target.write(json.dumps(dict(command=Path(row['argv'][0]).name,
                    start=row['start'], end=row['end'], exit=row['exit'])) + '\n')
    path = private / 'acceptance.jsonl'
    if path.exists():
        allowed_events = {'capture-ready', 'application-ready', 'created-tree-reference-saved',
                          'time-machine-start', 'time-machine-progress', 'time-machine-command-completed',
                          'acceptance-passed', 'acceptance-failed', 'resource-budget-stop'}
        allowed_fields = {'time', 'event', 'label', 'pid', 'seconds', 'short_headers', 'completed',
                          'free_bytes', 'tm_percent', 'tm_bytes', 'tm_total_bytes', 'store_kilobytes',
                          'noncapture_growth_estimate_bytes', 'store_allocated_bytes'}
        with path.open() as source, (public / 'events.jsonl').open('w') as target:
            for line in source:
                row = json.loads(line)
                if row['event'] in allowed_events:
                    target.write(json.dumps({k: v for k, v in row.items() if k in allowed_fields}) + '\n')
    with (public / 'application-errors.jsonl').open('w') as target:
        for path in sorted(private.glob('application-*.log')):
            with path.open(errors='replace') as source:
                for line in source:
                    for error in ('short client packet header', 'invalid transport format'):
                        if error in line:
                            ts = TIMESTAMP.search(line)
                            target.write(json.dumps(dict(time=ts[1] if ts else None, error=error)) + '\n')
    (public / 'export-policy.json').write_text(json.dumps(dict(
        policy='explicit metadata allowlist; no packet/header/signature/authentication bytes, private logs or arguments',
        raw_retention='separate authenticated ciphertext; encryption failure has no plaintext fallback')) + '\n')


if __name__ == '__main__':
    publish(sys.argv[1], sys.argv[2])
