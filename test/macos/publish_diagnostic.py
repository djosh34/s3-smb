#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Streaming fixed-marker extraction; full native/app/system logs stay encrypted."""
import collections
import json
from pathlib import Path
import re
import shutil
import sys

PHASES = frozenset(('Starting', 'MountingBackupVol', 'MountingDiskImage',
    'PreparingSourceVolumes', 'PreparingBackup', 'FindingChanges', 'CalculatingChanges',
    'CreatingSnapshot', 'Copying', 'Finishing', 'ThinningPreBackup', 'ThinningPostBackup',
    'UnmountingBackupVol', 'DeletingOldBackups', 'NotRunning'))
FAILURE = re.compile(r'\b(BACKUP_FAILED_[A-Z_]{1,80})(?:\s*\(([0-9]{1,5})\))?')
BAND = re.compile(r'Using a band size of ([0-9]+(?:\.[0-9]+)?) ([A-Z]{1,4}) '
                  r'\(on a volume with size of ([0-9]+(?:\.[0-9]+)?) ([A-Z]{1,4})\)')
TIMESTAMP = re.compile(r'"(?:time|timestamp)"\s*:\s*"([0-9T:.+ Z-]{10,40})"')


def phase(text):
    match = re.search(r'\bBackupPhase\s*=\s*"?([A-Za-z]+)"?\s*;', text)
    return match[1] if match and match[1] in PHASES else None


def publish(private, public):
    private, public = Path(private), Path(public)
    public.mkdir(parents=True, exist_ok=False)
    # Producers below emit only build identity, fixed classifications/numbers.
    for name in ('harness-revision', 'application-version', 'application-source-revision',
                 'application-tag-object', 'application-sha256.txt', 'native-build.txt',
                 'application-variant.txt', 'minio-revision', 'capture-command.json',
                 'framing-error-counts.json', 'measurement.json', 'platform.txt',
                 'workload-outcome.json', 'cleanup-outcome.json', 'exposure.json'):
        path = private / name
        if path.is_file():
            shutil.copyfile(path, public / name)
    # Reviewed capture producer supplies numeric metadata only; never its stderr.
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
    observed_phases = collections.Counter()
    path = private / 'acceptance.jsonl'
    if path.exists():
        allowed_events = {'capture-ready', 'application-ready', 'created-tree-reference-saved',
                          'time-machine-start', 'time-machine-progress', 'time-machine-command-completed',
                          'acceptance-passed', 'acceptance-failed'}
        allowed_fields = {'time', 'event', 'label', 'pid', 'seconds', 'short_headers', 'completed',
                          'free_bytes', 'tm_percent', 'tm_bytes', 'tm_total_bytes', 'store_kilobytes'}
        with path.open() as source, (public / 'events.jsonl').open('w') as target:
            for line in source:
                row = json.loads(line)
                if row['event'] not in allowed_events:
                    continue
                safe = {k: v for k, v in row.items() if k in allowed_fields}
                if row['event'] == 'time-machine-progress':
                    safe['tm_phase'] = phase(row.get('native_status', ''))
                    observed_phases[safe['tm_phase'] or 'unrecognized_or_absent'] += 1
                target.write(json.dumps(safe) + '\n')
    with (public / 'application-errors.jsonl').open('w') as target:
        for path in sorted(private.glob('application-*.log')):
            with path.open(errors='replace') as source:
                for line in source:
                    for error in ('short client packet header', 'invalid transport format'):
                        if error in line:
                            ts = TIMESTAMP.search(line)
                            target.write(json.dumps(dict(time=ts[1] if ts else None, error=error)) + '\n')
    failures = collections.Counter()
    codes = collections.Counter()
    files = lines = 0
    with (public / 'client-observations.jsonl').open('w') as target:
        for path in private.glob('*-log.log'):
            files += 1
            with path.open(errors='replace') as source:
                for line in source:
                    lines += 1
                    ts = TIMESTAMP.search(line)
                    timestamp = ts[1] if ts else None  # Never borrow another object's time.
                    for match in FAILURE.finditer(line):
                        failures[match[1]] += 1
                        code = int(match[2]) if match[2] else None
                        if code is not None:
                            codes[str(code)] += 1
                        target.write(json.dumps(dict(time=timestamp, failure_enum=match[1],
                            failure_code=code, source='fixed_native_log_marker')) + '\n')
                    for match in BAND.finditer(line):
                        target.write(json.dumps(dict(time=timestamp, band_value=float(match[1]),
                            band_unit=match[2], volume_value=float(match[3]), volume_unit=match[4])) + '\n')
    (public / 'client-outcome.json').write_text(json.dumps(dict(
        failure_enum_marker_counts=failures, failure_code_marker_counts=codes,
        phase_sample_counts=observed_phases, native_log_files_scanned=files,
        native_log_lines_scanned=lines, complete_client_outcome_proven=False,
        caveat='Fixed-marker observations only; zero markers is not proof of backup success.')) + '\n')
    (public / 'export-policy.json').write_text(json.dumps(dict(
        policy='explicit metadata allowlist; no packet/header/signature/authentication bytes, private logs or arguments',
        raw_retention='separate authenticated ciphertext; encryption failure has no plaintext fallback')) + '\n')


if __name__ == '__main__':
    publish(sys.argv[1], sys.argv[2])
