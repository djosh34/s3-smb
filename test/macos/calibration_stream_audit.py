#!/usr/bin/env python3
"""Offline independent audit of the known-pattern, one-flow TCP calibration.

No network operations, SMB parser, key handling or raw-file extraction.
"""
import bisect
from collections import Counter, deque
import hashlib
import json
import os
from pathlib import Path
import struct
import sys
import time

from window_audit import decode, MOD, HALF, MeasurementError

TOTAL = 19_000_000_000
PORT = 28464
EXPECTED = (TOTAL, 8)


def signed(new, old):
    return ((new - old + HALF) % MOD) - HALF


def lift(raw, base, reference):
    return reference + signed(raw, (base + reference) % MOD)


class Union:
    def __init__(self):
        self.ranges = []
        self.unique = 0
        self.max_ranges = 0

    def add(self, start, end):
        if start >= end:
            return 0
        rows = self.ranges
        i = bisect.bisect_left(rows, (start, -1))
        if i and rows[i - 1][1] >= start:
            i -= 1
        j, left, right, removed = i, start, end, 0
        while j < len(rows) and rows[j][0] <= right:
            left = min(left, rows[j][0])
            right = max(right, rows[j][1])
            removed += rows[j][1] - rows[j][0]
            j += 1
        added = right - left - removed
        rows[i:j] = [(left, right)]
        self.unique += added
        self.max_ranges = max(self.max_ranges, len(rows))
        if len(rows) > 65536:
            raise MeasurementError('interval-budget')
        return end - start - added

    def gaps(self, expected):
        out, pos = [], 0
        for left, right in self.ranges:
            if left > pos:
                out.append([pos, min(left, expected)])
            pos = max(pos, right)
        if pos < expected:
            out.append([pos, expected])
        return [v for v in out if v[0] < v[1]]


class Direction:
    def __init__(self):
        self.base = None
        self.high = 0
        self.fin = None
        self.last_ack = None
        self.max_ack = 0
        self.union = Union()
        self.counts = Counter()


class StrictOriginal:
    """Model the deployed C ordering rules, not the independent coverage verdict."""
    def __init__(self):
        self.d = [dict(next=0, ack=0, syn=False, fin=False, ack_seen=False, bytes=0) for _ in range(2)]
        self.failure = None
        self.context = deque(maxlen=5)
        self.after = []

    def observe(self, p, direction, record):
        cur, peer = self.d[direction], self.d[1 - direction]
        row = dict(record=record, direction=direction, flags=p.flags, payload_length=p.length,
                   seq_minus_current_next=signed(p.seq, cur['next']),
                   ack_minus_peer_next=signed(p.ack, peer['next']),
                   ack_minus_last_ack=signed(p.ack, cur['ack']) if cur['ack_seen'] else None)
        if self.failure is not None:
            if len(self.after) < 3:
                self.after.append(row)
            return
        reason = None
        if p.flags & 4 or p.flags & 0xc0:
            reason = 'rst_or_ecn'
        elif p.flags & 2:
            if cur['syn'] or p.length or p.flags & 1 or (direction == 1 and not p.flags & 16):
                reason = 'syn_state'
            else:
                cur['syn'], cur['next'] = True, (p.seq + 1) % MOD
        elif not cur['syn'] or p.seq != cur['next'] or (cur['fin'] and (p.length or p.flags & 1)):
            reason = 'exact_next_sequence_requirement'
        else:
            cur['next'] = (cur['next'] + p.length + bool(p.flags & 1)) % MOD
            cur['bytes'] += p.length
            cur['fin'] |= bool(p.flags & 1)
        if reason is None and p.flags & 16:
            if not peer['syn']:
                reason = 'ack_without_peer_syn'
            elif not cur['ack_seen'] and p.ack != peer['next']:
                reason = 'first_ack_not_peer_next'
            elif signed(p.ack, peer['next']) > 0:
                reason = 'ack_ahead_of_observed_peer_next'
            elif cur['ack_seen'] and signed(p.ack, cur['ack']) < 0:
                reason = 'ack_regression'
            else:
                cur['ack'], cur['ack_seen'] = p.ack, True
        elif reason is None and (not p.flags & 2 or direction):
            reason = 'missing_required_ack'
        if reason:
            self.failure = dict(**row, reason=reason, accepted_client_bytes=self.d[0]['bytes'],
                                accepted_server_bytes=self.d[1]['bytes'], preceding=list(self.context))
            # Fixed numeric diagnostic only; provisional until archive gates succeed.
            print(json.dumps(dict(stage='provisional_first_c_rejection', **self.failure)), flush=True)
        self.context.append(row)


class Flow:
    def __init__(self):
        self.d = [Direction(), Direction()]
        self.client_port = None
        self.records = 0
        self.counts = Counter()
        self.strict = StrictOriginal()

    def observe(self, p, payload):
        self.records += 1
        if p.src != bytes([127, 0, 0, 1]) or p.dst != p.src:
            raise MeasurementError('unexpected-calibration-address')
        if self.client_port is None:
            if p.dport != PORT or p.sport == PORT or p.flags & 0x17 != 2 or p.length:
                raise MeasurementError('missing-first-client-syn')
            self.client_port = p.sport
        if p.sport == self.client_port and p.dport == PORT:
            direction = 0
        elif p.sport == PORT and p.dport == self.client_port:
            direction = 1
        else:
            raise MeasurementError('unexpected-flow')
        self.strict.observe(p, direction, self.records)
        cur, peer = self.d[direction], self.d[1 - direction]
        cur.counts['packets'] += 1
        cur.counts['rst'] += bool(p.flags & 4)
        if p.flags & 2:
            base = (p.seq + 1) % MOD
            if cur.base is not None and cur.base != base:
                raise MeasurementError('reused-tuple-different-syn')
            if p.length or p.flags & 1:
                raise MeasurementError('syn-data-not-calibration')
            cur.base = base
            cur.counts['syn_packets'] += 1
        else:
            if cur.base is None:
                raise MeasurementError('unanchored-sequence')
            reference = cur.high + (cur.fin == cur.high)
            offset = lift(p.seq, cur.base, reference)
            if offset < 0 or offset > EXPECTED[direction] + 1:
                raise MeasurementError('sequence-outside-calibration-extent')
            if p.length:
                end = offset + len(payload)
                if end > EXPECTED[direction]:
                    raise MeasurementError('payload-outside-calibration-extent')
                cur.counts['payload_packets'] += 1
                cur.counts['payload_bytes_with_repeats'] += len(payload)
                cur.counts['payload_starts_behind_high'] += offset < cur.high
                cur.counts['payload_starts_ahead_high'] += offset > cur.high
                if direction == 0:
                    matched = payload.count(0) == len(payload)
                else:
                    matched = payload == struct.pack('!Q', TOTAL)[offset:end]
                cur.counts['pattern_mismatch_packets'] += not matched
                overlap = cur.union.add(offset, end)
                cur.counts['overlap_bytes'] += overlap
                cur.counts['overlap_packets'] += overlap > 0
                cur.high = max(cur.high, end)
            elif not p.flags & 1:
                cur.counts['pure_ack_sequence_behind_high'] += offset < reference
                cur.counts['pure_ack_sequence_ahead_high'] += offset > reference
            if p.flags & 1:
                end = offset + p.length
                if cur.fin is not None and cur.fin != end:
                    raise MeasurementError('inconsistent-fin-offset')
                cur.fin = end
                cur.counts['fin_packets'] += 1
        if p.flags & 16:
            if peer.base is None:
                raise MeasurementError('ack-before-peer-anchor')
            ack = lift(p.ack, peer.base, max(peer.high + (peer.fin == peer.high), cur.max_ack))
            if not 0 <= ack <= EXPECTED[1 - direction] + 1:
                raise MeasurementError('ack-outside-calibration-extent')
            if cur.last_ack is not None:
                cur.counts['ack_regressions_vs_previous'] += ack < cur.last_ack
            cur.counts['ack_below_prior_max'] += ack < cur.max_ack
            cur.counts['ack_ahead_of_current_peer_high'] += ack > peer.high + (peer.fin == peer.high)
            cur.last_ack, cur.max_ack = ack, max(cur.max_ack, ack)

    def result(self):
        directions = []
        complete = True
        for i, cur in enumerate(self.d):
            gaps = cur.union.gaps(EXPECTED[i])
            valid = (cur.base is not None and not gaps and cur.union.unique == EXPECTED[i]
                     and cur.fin == EXPECTED[i] and self.d[1 - i].max_ack == EXPECTED[i] + 1
                     and not cur.counts['rst'] and not cur.counts['pattern_mismatch_packets'])
            complete &= valid
            directions.append(dict(direction=i, expected_payload_bytes=EXPECTED[i],
                unique_payload_bytes=cur.union.unique, missing_bytes=sum(b-a for a,b in gaps),
                gap_count=len(gaps), first_gaps=gaps[:20], fin_data_offset=cur.fin,
                final_peer_max_ack_data_offset=self.d[1-i].max_ack,
                max_disjoint_ranges=cur.union.max_ranges, counts=dict(cur.counts),
                anchored_complete_expected_pattern_stream=valid))
        return dict(complete=bool(complete), records=self.records, client_port=self.client_port,
                    directions=directions, original_c_first_rejection=self.strict.failure,
                    original_c_next_three=self.strict.after,
                    limits=['one_known_synthetic_calibration_flow_only_not_smb_or_tm',
                            'nearest_sequence_unwrap_assumes_no_2_to_31_or_larger_unobserved_jump',
                            'known_pattern_validates_all_overlaps_without_payload_history',
                            'timestamps_do_not_establish_application_syscall_timing',
                            'complete_observed_stream_does_not_mean_every_redundant_packet_captured'])


class HashReader:
    def __init__(self, file):
        self.file, self.hash, self.count = file, hashlib.sha256(), 0

    def read(self, n=-1):
        if n < 0 or n > 1024 * 1024:
            raise MeasurementError('unbounded-read')
        b = self.file.read(n)
        self.hash.update(b)
        self.count += len(b)
        return b


def audit_pcap(f, size):
    f = HashReader(f)
    h = f.read(24)
    if len(h) != 24 or h[:4] not in (b'\xd4\xc3\xb2\xa1', b'\xa1\xb2\xc3\xd4'):
        raise MeasurementError('classic-microsecond-pcap-required')
    endian = '<' if h[:4] == b'\xd4\xc3\xb2\xa1' else '>'
    major, minor, _, _, snap, link = struct.unpack(endian + 'HHIIII', h[4:])
    if (major, minor) != (2, 4) or link != 0 or snap != 262144:
        raise MeasurementError('unexpected-pcap-format')
    flow = Flow()
    previous = None
    while f.count < size:
        h = f.read(16)
        if len(h) != 16:
            raise MeasurementError('short-record-header')
        sec, frac, cap, wire = struct.unpack(endian + 'IIII', h)
        if cap != wire or cap > snap or cap < 44 or f.count + cap > size or frac >= 1_000_000:
            raise MeasurementError('truncated-or-invalid-record')
        b = f.read(cap)
        if len(b) != cap:
            raise MeasurementError('short-record-body')
        timestamp = sec * 1_000_000_000 + frac * 1000
        if previous is not None:
            flow.counts['timestamp_regressions'] += timestamp < previous
        previous = timestamp
        if int.from_bytes(b[:4], 'little' if endian == '<' else 'big') != 2 or b[4] >> 4 != 4:
            raise MeasurementError('expected-ipv4-loopback')
        iptotal = int.from_bytes(b[6:8], 'big')
        if 4 + iptotal != cap:
            raise MeasurementError('unexpected-loopback-padding')
        p = decode(b[:256], cap, link, endian, timestamp)
        if p is None:
            raise MeasurementError('non-tcp-record')
        payload = b[cap-p.length:] if p.length else b''
        flow.observe(p, payload)
        if flow.records > 2_000_000:
            raise MeasurementError('packet-budget')
    result = flow.result()
    result.update(pcap_bytes=f.count, pcap_sha256=f.hash.hexdigest(), stats=dict(flow.counts))
    return result


def main():
    # The only external parser is the unchanged reviewed authenticated archive reader.
    sys.path.insert(0, '/tmp/s3-smb-swarm-64/capture-retention-operations')
    from strict_archive import walk_archive, META_TOTAL
    import resource
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    resource.setrlimit(resource.RLIMIT_AS, (512 * 1024**2, 512 * 1024**2))
    os.umask(0o077)
    root, output = Path(sys.argv[1]), Path(sys.argv[2])
    output.mkdir(mode=0o700, exist_ok=False)
    release = json.loads((root / 'AUTHENTICATED-SPOOL-RELEASE.json').read_text())
    if release['cms_auth_exit'] != 0 or not release['cms_format_valid'] or not release['frozen_input_no_writers']:
        raise MeasurementError('authentication-gate')
    spool = root / 'archive.verified.tar.gz'
    before = spool.stat()
    if spool.is_symlink() or before.st_mode & 0o777 != 0o400 or before.st_size != release['verified_compressed_bytes']:
        raise MeasurementError('spool-mode-size')
    if str(spool) != release['verified_compressed_path']:
        raise MeasurementError('spool-path')
    expected_raw = release['expected_raw_bytes']
    result, raw_seen, logs, files = None, 0, 0, 0
    def limit(name):
        if name == 'private-traffic.pcap':
            return expected_raw
        if name == 'evidence' or name.startswith('evidence/'):
            return 1024**3
        raise MeasurementError('unexpected-archive-root')
    def sink(info, file):
        nonlocal result, raw_seen, logs, files
        if info.name == 'private-traffic.pcap':
            raw_seen += 1
            if raw_seen != 1 or info.size != expected_raw:
                raise MeasurementError('raw-member-identity')
            result = audit_pcap(file, info.size)
            return {}
        files += 1
        logs += info.size
        if files > 4096 or logs > 1024**3:
            raise MeasurementError('private-log-budget')
        for _ in iter(lambda: file.read(1024 * 1024), b''):
            pass
        return {}
    start = time.monotonic()
    with spool.open('rb') as source:
        stream = HashReader(source)
        gates = walk_archive(stream, total_limit=expected_raw + 1024**3 + META_TOTAL + 16*1024**2,
                             per_file_limit=expected_raw, file_limit=limit, sink=sink)
        if stream.count != before.st_size or stream.hash.hexdigest() != release['verified_compressed_sha256']:
            raise MeasurementError('consumed-spool-hash-mismatch')
    after = spool.stat()
    if (before.st_ino, before.st_size, before.st_mtime_ns, before.st_mode) != (after.st_ino, after.st_size, after.st_mtime_ns, after.st_mode):
        raise MeasurementError('spool-changed')
    if raw_seen != 1 or not gates['gzip_eof_crc_ok'] or not gates['tar_two_zero_end_and_padding_ok']:
        raise MeasurementError('archive-final-gates')
    result.update(archive_authenticated_before_parse=True, consumed_spool_hash_match=True,
                  gzip_crc_and_tar_end_valid=True, input_stable=True, raw_files_created=0,
                  private_log_files=files, private_log_bytes=logs,
                  elapsed_seconds=round(time.monotonic()-start, 3),
                  maxrss_kib=resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)
    (output / 'summary.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(dict(stage='final', complete=result['complete'], records=result['records'],
                         archive_gates=True, seconds=result['elapsed_seconds'])), flush=True)
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        print(json.dumps(dict(stage='failed_no_final_publication', error_class=type(error).__name__)), flush=True)
        raise SystemExit(1)
