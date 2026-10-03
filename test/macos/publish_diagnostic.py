#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Publish an explicit metadata allowlist. All native/app/system logs stay private."""
import json
from pathlib import Path
import re
import shutil
import sys


def publish(private, public):
    private, public = Path(private), Path(public)
    public.mkdir(parents=True, exist_ok=False)
    # These producers contain no command credentials, log messages or payloads.
    for name in ('harness-revision', 'application-version', 'application-source-revision',
                 'application-tag-object', 'application-sha256.txt', 'native-build.txt',
                 'minio-revision', 'tcpdump-stderr.log', 'capture-health.jsonl', 'capture-command.json',
                 'capture-stop.json', 'framing-error-counts.json', 'measurement.json', 'platform.txt'):
        path = private / name
        if path.is_file():
            shutil.copyfile(path, public / name)
    for name in ('frames.jsonl', 'capture-summary.json'):
        path = private / 'framing' / name
        if path.is_file():
            (public / 'framing').mkdir(exist_ok=True)
            if name == 'capture-summary.json':
                shutil.copyfile(path, public / 'framing' / name)
            else:
                with (public / 'framing' / name).open('w') as f:
                    for line in path.read_text().splitlines():
                        row = json.loads(line)
                        header = row.pop('header', None)
                        if row.get('previous'):
                            row['previous'].pop('header', None)
                        if row.get('event') == 'invalid-frame':
                            row['reason'] = 'short-payload' if header and header.startswith('00') else 'nonzero-type'
                            if row['reason'] == 'nonzero-type':
                                row.pop('declared_length', None)
                        f.write(json.dumps(row, separators=(',', ':')) + '\n')
    # Native command timings without argv, output, paths or exception strings.
    path = private / 'commands.jsonl'
    if path.exists():
        with (public / 'command-timings.jsonl').open('w') as f:
            for line in path.read_text().splitlines():
                row = json.loads(line)
                f.write(json.dumps(dict(command=Path(row['argv'][0]).name,
                    start=row['start'], end=row['end'], exit=row['exit'])) + '\n')
    path = private / 'acceptance.jsonl'
    if path.exists():
        allowed_events = {'capture-ready', 'application-ready', 'created-tree-reference-saved',
                          'time-machine-start', 'time-machine-progress', 'time-machine-command-completed',
                          'acceptance-passed', 'acceptance-failed'}
        allowed_fields = {'time', 'event', 'label', 'pid', 'seconds', 'short_headers', 'completed',
                          'free_bytes', 'tm_percent', 'tm_bytes', 'tm_total_bytes', 'baseline', 'store_kilobytes'}
        with (public / 'events.jsonl').open('w') as f:
            for line in path.read_text().splitlines():
                row = json.loads(line)
                if row['event'] in allowed_events:
                    f.write(json.dumps({k: v for k, v in row.items() if k in allowed_fields}) + '\n')
    # Exact known errors, not arbitrary application error text.
    with (public / 'application-errors.jsonl').open('w') as f:
        for path in sorted(private.glob('application-*.log')):
            for line in path.read_text(errors='replace').splitlines():
                for error in ('short client packet header', 'invalid transport format'):
                    if error in line:
                        ts = re.search(r'"time"\s*:\s*"([0-9T:.+Z-]+)"', line)
                        f.write(json.dumps(dict(file=path.name, time=ts.group(1) if ts else None, error=error)) + '\n')
    # Deliberately narrow safe extracts from unified logs: no arbitrary messages.
    patterns = [r'Using a band size of [0-9.]+ [A-Z]+ \(on a volume with size of [0-9.]+ [A-Z]+\)',
                r'Disk image resized to [0-9]+ bytes', r'BACKUP_FAILED_DISCONNECTED_NETWORK(?: \(26\))?',
                r'BACKUP_FAILED_[A-Z_]+', r'Backup completed successfully',
                r'Successfully created [0-9]+(?:\.[0-9]+)? [A-Z]+']
    with (public / 'client-observations.jsonl').open('w') as f:
        for path in private.glob('*-log.log'):
            try:
                rows = json.loads(path.read_text())
            except (ValueError, UnicodeError):
                continue
            if not isinstance(rows, list):
                continue
            for row in rows:
                message = row.get('eventMessage', '')
                for pattern in patterns:
                    match = re.search(pattern, message)
                    if match:
                        f.write(json.dumps(dict(time=row.get('timestamp'), observation=match.group(0))) + '\n')
    (public / 'export-policy.json').write_text(json.dumps(dict(
        policy='explicit metadata allowlist; no raw packets, stream tails, native output, full application/system logs, configuration or command arguments',
        limitation='raw pcap and detailed native logs are private ephemeral runner scratch')) + '\n')


if __name__ == '__main__':
    publish(sys.argv[1], sys.argv[2])
