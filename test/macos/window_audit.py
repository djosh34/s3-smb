#!/usr/bin/env python3
"""Passive, payload-silent TCP receive-window measurements from classic pcap.

Reads packet headers only, never parses SMB, never transmits. Numeric outputs
are observations, not continuous stall durations or application-read evidence.
Only run against a resource-admitted, authenticated, stable local capture.
"""
import argparse
from collections import Counter
from dataclasses import dataclass, field
import json
from pathlib import Path
import stat
import struct
import sys

MOD = 1 << 32
HALF = MOD // 2
NS = 1_000_000_000


class MeasurementError(Exception):
    pass


@dataclass
class Packet:
    t: int
    src: bytes
    dst: bytes
    sport: int
    dport: int
    seq: int
    ack: int
    flags: int
    window: int
    length: int
    scale: object = None
    # Header equality is NOT packet equality or proof of duplicate tapping.
    identity: bytes = b''


def packets(path, stats):
    """Sequential offsets, bounded header reads; skip rather than copy payload."""
    with open(path, 'rb') as f:
        before = f.stat() if hasattr(f, 'stat') else Path(path).stat()
        h = f.read(24)
        formats = {b'\xd4\xc3\xb2\xa1': ('<', 1000), b'\xa1\xb2\xc3\xd4': ('>', 1000),
                   b'\x4d\x3c\xb2\xa1': ('<', 1), b'\xa1\xb2\x3c\x4d': ('>', 1)}
        if len(h) != 24 or h[:4] not in formats:
            raise MeasurementError('invalid-classic-pcap')
        endian, scale = formats[h[:4]]
        major, minor, _, _, snaplen, link = struct.unpack(endian + 'HHIIII', h[4:])
        if (major, minor) != (2, 4) or link not in (0, 1, 101, 108):
            raise MeasurementError('unsupported-pcap')
        stats['link_type'] = link
        stats['snaplen'] = snaplen
        last = -1
        pos = 24
        while pos < before.st_size:
            header = f.read(16)
            if len(header) != 16:
                raise MeasurementError('partial-record-header')
            sec, frac, cap, wire = struct.unpack(endian + 'IIII', header)
            pos += 16
            if cap > wire or cap > snaplen or cap > 16 * 1024 * 1024 or pos + cap > before.st_size:
                raise MeasurementError('invalid-record-size')
            t = sec * NS + frac * scale
            if frac * scale >= NS or t < last:
                raise MeasurementError('invalid-or-reversed-timestamp')
            last = t
            stats['records'] += 1
            stats['truncated_records'] += cap != wire
            head = f.read(min(cap, 256))
            if len(head) != min(cap, 256):
                raise MeasurementError('partial-packet-header')
            pos += cap
            f.seek(pos)
            packet = decode(head, cap, link, endian, t)
            if packet is None:
                stats['non_tcp_records'] += 1
            else:
                yield packet
        after = Path(path).stat()
        if (before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_ino, after.st_size, after.st_mtime_ns):
            raise MeasurementError('input-changed')


def decode(h, cap, link, endian, t):
    p = 0
    if link in (0, 108):
        p = 4
    elif link == 1:
        if len(h) < 14:
            raise MeasurementError('short-link-header')
        kind = int.from_bytes(h[12:14], 'big')
        p = 14
        for _ in range(2):
            if kind not in (0x8100, 0x88a8):
                break
            if len(h) < p + 4:
                raise MeasurementError('short-vlan')
            kind = int.from_bytes(h[p + 2:p + 4], 'big')
            p += 4
        if kind not in (0x0800, 0x86dd):
            return None
    if len(h) < p + 20:
        raise MeasurementError('short-ip-header')
    version = h[p] >> 4
    if version == 4:
        ihl = (h[p] & 15) * 4
        total = int.from_bytes(h[p + 2:p + 4], 'big')
        if int.from_bytes(h[p + 6:p + 8], 'big') & 0x3fff:
            raise MeasurementError('fragmented-ip-unsupported')
        if h[p + 9] != 6:
            return None
        if ihl < 20 or total < ihl + 20 or p + total > cap:
            raise MeasurementError('invalid-ip-length')
        src, dst = h[p + 12:p + 16], h[p + 16:p + 20]
        end = p + total
        p += ihl
    elif version == 6:
        if len(h) < p + 40:
            raise MeasurementError('short-ipv6-header')
        if h[p + 6] != 6:
            raise MeasurementError('ipv6-extensions-or-nontcp-unsupported')
        end = p + 40 + int.from_bytes(h[p + 4:p + 6], 'big')
        if end > cap:
            raise MeasurementError('truncated-ipv6')
        src, dst = h[p + 8:p + 24], h[p + 24:p + 40]
        p += 40
    else:
        raise MeasurementError('unsupported-ip-version')
    if len(h) < p + 20:
        raise MeasurementError('short-tcp-header')
    sport, dport, seq, ack, off, flags, win = struct.unpack('!HHIIBBH', h[p:p + 16])
    size = (off >> 4) * 4
    if size < 20 or p + size > end or p + size > len(h):
        raise MeasurementError('invalid-tcp-header-size')
    shift = None
    if flags & 2:
        q = p + 20
        while q < p + size:
            kind = h[q]
            if kind == 0:
                break
            if kind == 1:
                q += 1
                continue
            if q + 2 > p + size or h[q + 1] < 2 or q + h[q + 1] > p + size:
                raise MeasurementError('invalid-tcp-option')
            if kind == 3:
                if h[q + 1] != 3 or shift is not None:
                    raise MeasurementError('invalid-window-scale-option')
                shift = min(14, h[q + 2])
            q += h[q + 1]
    return Packet(t, src, dst, sport, dport, seq, ack, flags, win,
                  end - p - size, shift, h[p:p + size])


def forward(new, old):
    delta = (new - old) % MOD
    return delta if delta < HALF else None


@dataclass
class Direction:
    counts: Counter = field(default_factory=Counter)
    windows: Counter = field(default_factory=Counter)
    syn: object = None
    scale: object = None
    last_ack: object = None
    last_window: object = None
    last_time: object = None
    last_identity: object = None
    last_nonzero: object = None
    active: object = None
    top: list = field(default_factory=list)
    bins: dict = field(default_factory=dict)

    def close_episode(self, next_nonzero=None):
        e = self.active
        if e is None:
            return
        e['next_nonzero_ns'] = next_nonzero
        e['zero_sample_span_ns'] = e['last_zero_ns'] - e['first_zero_ns']
        e['left_censored'] = e['previous_nonzero_ns'] is None
        e['right_censored'] = next_nonzero is None
        e['continuous_duration_lower_bound_ns'] = 0
        e['bracketing_envelope_ns'] = (None if e['left_censored'] or e['right_censored']
                                       else next_nonzero - e['previous_nonzero_ns'])
        self.counts['zero_observation_runs'] += 1
        self.counts['left_censored_runs'] += e['left_censored']
        self.counts['right_censored_runs'] += e['right_censored']
        self.counts['zero_sample_spans_ge_5s'] += e['zero_sample_span_ns'] >= 5 * NS
        self.counts['max_zero_sample_span_ns'] = max(self.counts['max_zero_sample_span_ns'], e['zero_sample_span_ns'])
        self.counts['max_zero_intersample_gap_ns'] = max(self.counts['max_zero_intersample_gap_ns'], e['max_zero_intersample_gap_ns'])
        self.top.append(e)
        self.top.sort(key=lambda v: v['zero_sample_span_ns'], reverse=True)
        self.top = self.top[:20]
        self.active = None

    def observe(self, p, origin, peer):
        t = p.t - origin
        self.counts['packets'] += 1
        self.counts['payload_bytes_observed_with_repeats'] += p.length
        self.counts['syn'] += bool(p.flags & 2)
        self.counts['fin'] += bool(p.flags & 1)
        self.counts['rst'] += bool(p.flags & 4)
        b = self.bins.setdefault(t // NS, Counter())
        b['packets'] += 1
        b['payload_bytes_with_repeats'] += p.length
        if self.last_identity == p.identity and self.last_time is not None and t - self.last_time <= 1_000_000:
            self.counts['adjacent_equal_tcp_headers_within_1ms'] += 1
        self.last_identity, self.last_time = p.identity, t
        if p.flags & 2:
            if self.syn is not None and self.syn != p.seq:
                raise MeasurementError('multiple-syn-sequences-in-generation')
            self.syn, self.scale = p.seq, p.scale
            return
        if p.flags & 4 or not p.flags & 16:
            self.counts['non_window_records'] += 1
            return
        advance = 0
        if self.last_ack is not None:
            delta = forward(p.ack, self.last_ack)
            if delta is None:
                self.counts['backward_ack_observations'] += 1
            else:
                advance = delta
                self.last_ack = p.ack
        else:
            self.last_ack = p.ack
        self.counts['ack_forward_bytes_minimum'] += advance
        b['ack_forward_bytes_minimum'] += advance
        if peer.last_window == 0 and p.length <= 1 and peer.last_ack is not None:
            if p.seq in (peer.last_ack, (peer.last_ack - 1) % MOD):
                self.counts['probe_or_keepalive_candidates_after_peer_zero'] += 1
                self.counts['probe_candidates_payload_bytes'] += p.length
        self.windows[p.window] += 1
        if p.window == 0:
            b['zero_window_samples'] += 1
            if self.active is None:
                self.active = dict(first_zero_ns=t, last_zero_ns=t,
                    previous_nonzero_ns=self.last_nonzero, zero_samples=0,
                    max_zero_intersample_gap_ns=0, ack_forward_between_zero_samples=0)
            e = self.active
            if e['zero_samples']:
                e['max_zero_intersample_gap_ns'] = max(e['max_zero_intersample_gap_ns'], t - e['last_zero_ns'])
                e['ack_forward_between_zero_samples'] += advance
            e['zero_samples'] += 1
            e['last_zero_ns'] = t
        else:
            b['nonzero_window_samples'] += 1
            self.close_episode(t)
            self.last_nonzero = t
        self.last_window = p.window

    def summary(self, peer):
        self.close_episode()
        factor = None if self.syn is None or peer.syn is None else (self.scale if self.scale is not None and peer.scale is not None else 0)
        count = sum(self.windows.values())
        def quantile(q):
            n = 0
            for value, c in sorted(self.windows.items()):
                n += c
                if n >= max(1, count * q):
                    return value
            return None
        raw = dict(min=min(self.windows) if count else None, median=quantile(.5),
                   p95=quantile(.95), max=max(self.windows) if count else None)
        return dict(counts=dict(self.counts), syn_observed=self.syn is not None,
                    offered_scale=self.scale, negotiated_scale=factor,
                    window_samples=count, zero_window_samples=self.windows[0],
                    raw_window_sample_quantiles=raw,
                    scaled_window_sample_quantiles={k: None if factor is None or v is None else v << factor for k, v in raw.items()},
                    top_zero_observation_runs=self.top)


class Audit:
    def __init__(self, port):
        self.port = port
        self.origin = None
        self.last = None
        self.flows = []
        self.current = {}
        self.stats = Counter()

    def add(self, p):
        if p.sport != self.port and p.dport != self.port:
            self.stats['other_port_packets'] += 1
            return
        if self.origin is None:
            self.origin = p.t
        self.last = p.t
        server = p.sport == self.port
        key = (p.dst, p.dport, p.src, p.sport) if server else (p.src, p.sport, p.dst, p.dport)
        f = self.current.get(key)
        new_syn = bool(p.flags & 2 and not p.flags & 16)
        if f is None or (new_syn and f['client'].syn is not None and f['client'].syn != p.seq):
            generation = 1 if f is None else f['generation'] + 1
            f = dict(flow=len(self.flows) + 1, client_port=key[1], server_port=key[3], generation=generation,
                     client=Direction(), server=Direction(), first_ns=p.t - self.origin)
            self.flows.append(f)
            self.current[key] = f
        f['last_ns'] = p.t - self.origin
        d, peer = (f['server'], f['client']) if server else (f['client'], f['server'])
        d.observe(p, self.origin, peer)

    def output(self):
        out = []
        bins = []
        for f in self.flows:
            row = {k: v for k, v in f.items() if k not in ('client', 'server')}
            for name in ('client', 'server'):
                d = f[name]
                row[name + '_advertised'] = d.summary(f['server' if name == 'client' else 'client'])
                for sec, b in sorted(d.bins.items()):
                    bins.append(dict(flow=f['flow'], advertiser=name, second=sec, **dict(b)))
            out.append(row)
        return dict(schema=1, capture_origin_unix_ns=self.origin, last_relative_ns=None if self.last is None else self.last - self.origin,
                    stats=dict(self.stats), flows=out,
                    limits=['no_payload_or_smb_parsing', 'not_socket_read_progress',
                            'global_capture_loss_does_not_locate_missing_window_updates',
                            'zero_sample_span_is_not_continuous_zero_duration',
                            'true_continuous_duration_lower_bound_is_zero_with_missing_updates',
                            'bracketing_envelope_only_bounds_possible_episode_between_observed_nonzero_windows',
                            'equal_tcp_headers_may_be_retransmission_or_loopback_duplicate_not_deduplicated',
                            'probe_candidates_not_confirmed_persist_probes',
                            'ack_unwrap_assumes_no_unobserved_2_to_31_or_larger_advance',
                            'window_quantiles_are_sample_weighted_not_time_weighted']), bins


def main():
    a = argparse.ArgumentParser(description=__doc__)
    a.add_argument('capture')
    a.add_argument('--server-port', type=int, default=1445)
    a.add_argument('--output', required=True)
    args = a.parse_args()
    try:
        mode = Path(args.capture).stat().st_mode
        if not stat.S_ISREG(mode) or mode & 0o077:
            raise MeasurementError('capture-must-be-private-regular-file')
        output = Path(args.output)
        output.mkdir(parents=True, exist_ok=False)
        audit = Audit(args.server_port)
        for p in packets(args.capture, audit.stats):
            audit.add(p)
        summary, bins = audit.output()
        (output / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
        with (output / 'seconds.jsonl').open('w') as f:
            for row in bins:
                f.write(json.dumps(row, separators=(',', ':')) + '\n')
        print(json.dumps(dict(ok=True, records=audit.stats['records'], flows=len(audit.flows))))
        return 0
    except Exception as error:
        # No exception message: parser/OS/JSON text could contain private bytes.
        print(json.dumps(dict(ok=False, error_class=type(error).__name__)))
        return 1


if __name__ == '__main__':
    sys.exit(main())
