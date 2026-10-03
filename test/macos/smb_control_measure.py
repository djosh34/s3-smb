#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Conservative control exposure/measurement verdict; not historical cause proof."""
import collections
import json
from pathlib import Path
import re


def measure(private, state, target_bytes, minimum_seconds, write_size):
    private = Path(private)
    summary_file = private / 'framing/capture-summary.json'
    summary = json.loads(summary_file.read_text()) if summary_file.exists() else {}
    drops_file = private / 'tcpdump-stderr.log'
    text = drops_file.read_text() if drops_file.exists() else ''
    drops = re.findall(r'(\d+) packets dropped by kernel', text)
    drops = int(drops[-1]) if drops else None
    sizes, protocols, signed = collections.Counter(), collections.Counter(), collections.Counter()
    requests, responses = {}, {}
    first = last = None
    resets = 0
    path = private / 'framing/frames.jsonl'
    if path.exists():
        with path.open() as source:
            for line in source:
                row = json.loads(line)
                if row['event'] == 'tcp-end' and row['flags'] & 4:
                    if state.get('write_start', float('inf')) <= row['timestamp'] <= state.get('write_end', 0):
                        resets += 1
                if row['event'] != 'frame':
                    continue
                protocols[row['protocol']] += 1
                for part in row.get('parts', []):
                    if part['command'] != 9:
                        continue
                    key = (row['connection'], part['message_id'])
                    if row['direction'] == 'c2s' and not part['flags'] & 1 and 'data_length' in part:
                        sizes[part['data_length']] += 1
                        requests[key] = part['data_length']
                        signed[bool(part['flags'] & 8)] += 1
                        first = min(first, row['timestamp']) if first is not None else row['timestamp']
                        last = max(last, row['timestamp']) if last is not None else row['timestamp']
                    elif row['direction'] == 's2c' and part['flags'] & 1:
                        if part['status_or_channel'] != 0x103:  # STATUS_PENDING is not final.
                            responses[key] = (part['status_or_channel'], part.get('write_count'))
    wire_bytes = sum(size * count for size, count in sizes.items())
    span = last - first if first is not None else 0
    mismatched = sum(responses.get(key) != (0, size) for key, size in requests.items())
    measurement_valid = bool(state.get('capture_alive_before_stop') and state.get('tcpdump_exit') == 0
        and drops == 0 and state.get('health_ok') and state.get('parser_exit') == 0
        and summary.get('observed_prefix_contiguous'))
    complete = bool(measurement_valid and summary.get('complete_reassembly'))
    exposure = bool(wire_bytes == target_bytes and span >= minimum_seconds
        and sizes.get(write_size, 0) * write_size >= .9 * target_bytes
        and not protocols['encrypted'] and not protocols['compressed'])
    clean = bool(complete and exposure and state.get('workload_ok') and state.get('unmount_ok')
        and not summary.get('invalid_frames') and not summary.get('unknown_protocol')
        and not summary.get('frames_with_validation_errors') and not mismatched and not resets)
    return dict(clean_control=clean, observed_prefix_valid=measurement_valid,
        complete_connection_capture=complete, exposure_target_met=exposure,
        tcpdump_exit=state.get('tcpdump_exit'), kernel_drops=drops,
        workload_ok=bool(state.get('workload_ok')), unmount_ok=bool(state.get('unmount_ok')),
        wire_write_bytes=wire_bytes, wire_write_span_seconds=span,
        wire_write_sizes=dict(sorted(sizes.items())), wire_write_signed_counts=dict(signed),
        write_requests_without_matching_success_count=mismatched,
        unmatched_write_responses=len(responses.keys() - requests.keys()),
        resets_during_workload=resets, protocols=dict(protocols),
        target=dict(bytes=target_bytes, minimum_seconds=minimum_seconds, write_size=write_size,
                    minimum_bytes_fraction_at_write_size=.9),
        caveat='Exposure target, not proven historical workload equivalence. Plain writes, not Time Machine; '
               'semantic checks partial; no causal inference from one clean control; raw capture ephemeral.')
