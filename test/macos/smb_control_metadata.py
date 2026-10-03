#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Export only framing metadata and a conservative independent-control verdict.

The source directory is PRIVATE. Neither byte-bearing tails nor parser logs are
copied. Header/magic hex at untrusted offsets are deliberately omitted.
"""
import json
from pathlib import Path
import sys


def export(source, destination):
    source, destination = Path(source), Path(destination)
    destination.mkdir(parents=True, exist_ok=True)
    stats = json.loads((source / 'capture-summary.json').read_text())
    summaries = []
    with (destination / 'frames.jsonl').open('w') as output:
        for line in (source / 'frames.jsonl').read_text().splitlines():
            record = json.loads(line)
            record.pop('header', None)
            record.pop('magic', None)
            if record.get('previous'):
                record['previous'].pop('header', None)
            record.pop('tail_file', None)
            if record.get('event') == 'stream-summary':
                summaries.append(record)
            output.write(json.dumps(record) + '\n')
    incomplete = any(stats.get(key, 0) for key in (
        'truncated_records', 'truncated_packets', 'unanchored_packets',
        'reassembly_overflow', 'streams_with_gaps', 'fragmented_ip_packets'))
    incomplete = incomplete or not summaries or any(
        not record['anchored'] or record['pending_bytes'] or record['partial_frame_bytes']
        for record in summaries)
    invalid = bool(stats.get('invalid_frames', 0))
    verdict = 'invalid-framing-observed' if invalid else 'inconclusive' if incomplete else 'complete-framing-observed'
    (destination / 'capture-summary.json').write_text(json.dumps(stats, indent=2) + '\n')
    (destination / 'verdict.json').write_text(json.dumps(dict(
        verdict=verdict, incomplete=bool(incomplete), invalid=invalid,
        client_bytes=sum(r['received'] for r in summaries if r['direction'] == 'c2s'),
        server_bytes=sum(r['received'] for r in summaries if r['direction'] == 's2c'),
        note='Raw bytes withheld; capture drops and PF tuple duplication must also be inspected.'
    ), indent=2) + '\n')
    return 0 if verdict == 'complete-framing-observed' else 1


if __name__ == '__main__':
    sys.exit(export(sys.argv[1], sys.argv[2]))
