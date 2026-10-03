#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Private transport headers versus independent pcap boundaries; public numeric result."""
import collections
import json
from pathlib import Path


def compare_frames(private):
    private = Path(private)
    frames = private / 'framing/frames.jsonl'
    app = private / 'application-frames.jsonl'
    if not app.exists() or not frames.exists():
        return dict(available=False)
    endpoints = collections.defaultdict(set)
    captured = {}
    with frames.open() as f:
        for line in f:
            row = json.loads(line)
            if row['event'] == 'connection-direction' and row['direction'] == 'c2s':
                endpoints[(f"{row['source'][0]}:{row['source'][1]}",
                           f"{row['destination'][0]}:{row['destination'][1]}")].add(row['connection'])
            elif row['event'] in ('frame', 'invalid-frame'):
                captured[(row['connection'], row['direction'], row['offset'])] = (row['header'], row['event'])
    counts = collections.Counter()
    first = []
    with app.open() as f:
        for line in f:
            row = json.loads(line)
            if row['event'] not in ('read-header', 'write-header'):
                continue
            counts['application_header_events'] += 1
            if row['actual'] != 4:
                counts['partial_header_io'] += 1
                continue
            ids = endpoints.get((row['remote'], row['local']), set())
            if len(ids) != 1:
                counts['ambiguous_or_missing_flow'] += 1
                continue
            direction = 'c2s' if row['event'] == 'read-header' else 's2c'
            actual = captured.get((next(iter(ids)), direction, row['offset']))
            if actual is None:
                verdict = 'no-captured-boundary-at-application-offset'
            elif actual[0] != row['header']:
                verdict = 'different-header-at-same-offset'
            else:
                counts['matching_headers'] += 1
                if actual[1] == 'invalid-frame':
                    counts['matching_invalid_headers'] += 1
                continue
            counts[verdict] += 1
            if len(first) < 20:
                first.append(dict(verdict=verdict, time=row['time'], connection=row['connection'],
                                  remote=row['remote'], local=row['local'], direction=direction,
                                  offset=row['offset'], previous_declared_length=row['previous_declared_length']))
    result = dict(available=True, counts=dict(counts), first_discrepancies=first,
                  limitation='Boundary/header comparison only; matching invalid stream does not exonerate prior server responses. Reused tuples and missing boundaries remain ambiguous. Capture quality must be evaluated separately.')
    (private / 'application-capture-comparison.json').write_text(json.dumps(result, indent=2) + '\n')
    return result
