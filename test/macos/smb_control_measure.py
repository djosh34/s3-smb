#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Conservative control exposure/measurement verdict; not historical cause proof."""
import collections
import json
from pathlib import Path
import re


def measure(private, state, target_bytes, minimum_seconds):
    private = Path(private)
    summary_file = private / 'framing/capture-summary.json'
    summary = json.loads(summary_file.read_text()) if summary_file.exists() else {}
    drops_file = private / 'tcpdump-stderr.log'
    text = drops_file.read_text() if drops_file.exists() else ''
    drops = re.findall(r'(\d+) packets dropped by kernel', text)
    drops = int(drops[-1]) if drops else None
    sizes, protocols, signed = collections.Counter(), collections.Counter(), collections.Counter()
    requests, responses, outstanding = {}, {}, {}
    peak_requests = peak_bytes = 0
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
                        if key not in responses:
                            outstanding[key] = part['data_length']
                            peak_requests = max(peak_requests, len(outstanding))
                            peak_bytes = max(peak_bytes, sum(outstanding.values()))
                        signed[bool(part['flags'] & 8)] += 1
                        first = min(first, row['timestamp']) if first is not None else row['timestamp']
                        last = max(last, row['timestamp']) if last is not None else row['timestamp']
                    elif row['direction'] == 's2c' and part['flags'] & 1:
                        if part['status_or_channel'] != 0x103:  # STATUS_PENDING is not final.
                            responses[key] = (part['status_or_channel'], part.get('write_count'))
                            outstanding.pop(key, None)
    wire_bytes = sum(size * count for size, count in sizes.items())
    span = last - first if first is not None else 0
    mismatched = sum(responses.get(key) != (0, size) for key, size in requests.items())
    measurement_valid = bool(state.get('capture_alive_before_stop') and state.get('tcpdump_exit') == 0
        and drops == 0 and state.get('health_ok') and state.get('parser_exit') == 0
        and summary.get('observed_prefix_contiguous'))
    complete = bool(measurement_valid and summary.get('complete_reassembly'))
    expected_fractions = {262144: 2/3, 1048576: 1/3}
    fractions = {size: sizes.get(size, 0) * size / max(1, wire_bytes) for size in expected_fractions}
    size_mix = (sum(fractions.values()) >= .9 and
                all(abs(fractions[size] - share) <= .1 for size, share in expected_fractions.items()))
    exposure = bool(wire_bytes == target_bytes and span >= minimum_seconds and size_mix
        and not protocols['encrypted'] and not protocols['compressed'])
    clean = bool(complete and exposure and state.get('workload_ok') and state.get('unmount_ok')
        and not summary.get('invalid_frames') and not summary.get('unknown_protocol')
        and not summary.get('frames_with_validation_errors') and not mismatched and not resets
        and not (responses.keys() - requests.keys()))
    return dict(clean_control=clean, observed_prefix_valid=measurement_valid,
        complete_connection_capture=complete, exposure_target_met=exposure,
        tcpdump_exit=state.get('tcpdump_exit'), kernel_drops=drops,
        workload_ok=bool(state.get('workload_ok')), unmount_ok=bool(state.get('unmount_ok')),
        wire_write_bytes=wire_bytes, wire_write_span_seconds=span,
        wire_write_sizes=dict(sorted(sizes.items())), wire_write_size_byte_fractions=fractions,
        wire_write_signed_counts=dict(signed),
        observed_peak_outstanding_requests=peak_requests, observed_peak_outstanding_bytes=peak_bytes,
        concurrency_caveat='Parsed request completion to parsed final response; global across handles; '
                           'not same-handle concurrency; reassembly ordering can undercount.',
        write_requests_without_matching_success_count=mismatched,
        unmatched_write_responses=len(responses.keys() - requests.keys()),
        resets_during_workload=resets, protocols=dict(protocols),
        target=dict(bytes=target_bytes, minimum_seconds=minimum_seconds,
                    write_size_byte_fractions=expected_fractions, fraction_tolerance=.1,
                    minimum_bytes_fraction_at_target_sizes=.9,
                    source='approximate large-WRITE mix from uncertain nonfailing rc7 ring alignment; '
                           'not a recovered historical failing workload'),
        caveat='Exposure target, not proven historical workload equivalence. Plain writes, not Time Machine; '
               'semantic checks partial; no causal inference from one clean control; raw capture ephemeral.')
