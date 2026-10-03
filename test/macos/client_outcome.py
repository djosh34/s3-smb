#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Fixed-field observations, not a backup-success or recovery verdict.

Read only runner-private command output. Never forward log text, paths or errors.
The caller supplies command exit/timeout and attempt boundaries independently.
"""
import argparse
import json
import math
from pathlib import Path, PurePosixPath
import re


MAX_INPUT = 16 * 1024 * 1024
CLOCK_WARNING = 'Wall Clock adjustment detected - results might be strange while using --end'
PHASES = frozenset(('Starting', 'FindingBackupVol', 'MountingBackupVol', 'MountingDiskImage',
                   'PreparingSourceVolumes', 'FindingChanges', 'Copying',
                   'Finishing', 'ThinningPreBackup', 'ThinningPostBackup'))
UNITS = frozenset(('B', 'KB', 'MB', 'GB', 'TB', 'PB'))


def status_observations(text):
    result = dict(parse='invalid', running=None, phase='unknown', percent=None,
                  bytes=None, total_bytes=None)
    running = re.findall(r'(?m)^\s*Running\s*=\s*([01])\s*;', text)
    if len(running) != 1:
        return result
    result.update(parse='ok', running=int(running[0]))
    phases = re.findall(r'(?m)^\s*BackupPhase\s*=\s*"?([A-Za-z]+)"?\s*;', text)
    if len(phases) == 1:
        result['phase'] = phases[0] if phases[0] in PHASES else 'other'
    for source, target in (('Percent', 'percent'), ('bytes', 'bytes'), ('totalBytes', 'total_bytes')):
        values = re.findall(rf'(?m)^\s*{source}\s*=\s*"?([0-9.eE+-]+)"?\s*;', text)
        if len(values) != 1:
            continue
        try:
            value = float(values[0]) if target == 'percent' else int(values[0])
        except ValueError:
            continue
        if 0 <= value <= (1 if target == 'percent' else 2**63 - 1) and math.isfinite(value):
            result[target] = value
    return result


def unified_observations(text):
    result = dict(parse='invalid', clock_warning=False, records=None,
                  backup_process_records=None, disconnected_network_records=None,
                  other_backup_failure_records=None, backup_success_records=None,
                  band_observations=None, band_value=None, band_unit=None,
                  image_resize_observations=None, image_bytes=None)
    text = text.lstrip()
    if text.startswith(CLOCK_WARNING + '\n') or text.startswith(CLOCK_WARNING + '\r\n'):
        result['clock_warning'] = True
        text = text.split('\n', 1)[1]
    try:
        rows = json.loads(text)
    except (ValueError, RecursionError):
        return result
    if not isinstance(rows, list) or any(not isinstance(row, dict) for row in rows):
        return result
    result.update(parse='ok', records=len(rows), backup_process_records=0,
                  disconnected_network_records=0, other_backup_failure_records=0,
                  backup_success_records=0, band_observations=0, image_resize_observations=0)
    for row in rows:
        process, message = row.get('processImagePath'), row.get('eventMessage')
        if not isinstance(process, str) or not isinstance(message, str):
            continue
        if PurePosixPath(process).name not in ('backupd', 'backupd-helper'):
            continue
        result['backup_process_records'] += 1
        # Each counter counts records, not attempts; a record may mention both outcomes.
        if re.search(r'\bBACKUP_FAILED_DISCONNECTED_NETWORK\b', message):
            result['disconnected_network_records'] += 1
        elif re.search(r'\bBACKUP_FAILED_[A-Z_]+\b', message):
            result['other_backup_failure_records'] += 1
        if 'Backup completed successfully' in message:
            result['backup_success_records'] += 1
        band = re.search(r'Using a band size of ([0-9]+(?:\.[0-9]+)?) ([A-Z]+)\b', message)
        if band and band[2] in UNITS:
            value = float(band[1])
            if math.isfinite(value) and 0 <= value <= 2**63 - 1:
                result['band_observations'] += 1
                result.update(band_value=value, band_unit=band[2])
        image = re.search(r'Disk image resized to ([0-9]{1,19}) bytes\b', message)
        if image and int(image[1]) <= 2**63 - 1:
            result['image_resize_observations'] += 1
            result['image_bytes'] = int(image[1])
    return result


def collect(kind, path):
    """Bounded memory; preserve the original on every error, emit no exception text."""
    try:
        with Path(path).open('rb') as source:
            data = source.read(MAX_INPUT + 1)
    except OSError:
        return dict(parse='read-error')
    if len(data) > MAX_INPUT:
        return dict(parse='oversize')
    try:
        text = data.decode('utf-8')
    except UnicodeError:
        return dict(parse='encoding-error')
    return status_observations(text) if kind == 'status' else unified_observations(text)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('kind', choices=('status', 'unified'))
    parser.add_argument('private_input')
    args = parser.parse_args()
    result = collect(args.kind, args.private_input)
    print(json.dumps(result, allow_nan=False, sort_keys=True))
    return 0 if result['parse'] == 'ok' else 2


if __name__ == '__main__':
    raise SystemExit(main())
