#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Private pcap -> private framing observations, never full payload/stream dumps.

Usage: framing_capture.py INPUT.pcap PRIVATE_OUTPUT_DIRECTORY
Suspect header4 bytes remain private: publish_diagnostic.py removes them before
upload. Absolute directional offsets begin at SYN+1. Numeric READ/WRITE metadata
permits partial structural analysis; no payload digests are emitted.
A complete TCP framing audit is NOT a complete SMB semantic correctness proof.
"""
import collections
import json
from pathlib import Path
import socket
import struct
import sys

MAX_FRAME = 0xffffff
HISTORY = 32 * 1024 * 1024


def relative(seq, expected):
    return (seq - expected + 2**31) % 2**32 - 2**31


def u16(data, at):
    return int.from_bytes(data[at:at + 2], 'little')


def u32(data, at):
    return int.from_bytes(data[at:at + 4], 'little')


def u64(data, at):
    return int.from_bytes(data[at:at + 8], 'little')


class Audit:
    def __init__(self, output):
        self.output = Path(output)
        self.output.mkdir(parents=True, exist_ok=True)
        self.log = (self.output / 'frames.jsonl').open('w')
        self.streams = {}
        self.connections = {}
        self.stats = collections.Counter()
        self.counter = 0
        self.summaries = []

    def event(self, **fields):
        self.log.write(json.dumps(fields, separators=(',', ':')) + '\n')

    def tcp(self, src, dst, sport, dport, seq, ack, flags, data, timestamp):
        if 1445 not in (sport, dport) and 445 not in (sport, dport):
            return
        client, server = ((src, sport), (dst, dport)) if dport in (1445, 445) else ((dst, dport), (src, sport))
        key = (client, server)
        direction = 'c2s' if dport in (1445, 445) else 's2c'
        syn = bool(flags & 2)
        conn = self.connections.get(key)
        old = self.streams.get((conn, direction))
        if conn is None or (syn and old is not None and old.initial is not None and old.initial != seq):
            self.counter += 1
            if self.counter > 1024:
                raise RuntimeError('capture connection resource bound exceeded')
            conn = f'conn{self.counter:04d}-{client[1]}'
            self.connections[key] = conn
        sk = (conn, direction)
        if sk not in self.streams:
            self.streams[sk] = Stream(self, conn, direction, seq if syn else None, timestamp)
            self.event(event='connection-direction', connection=conn, direction=direction,
                       source=[src, sport], destination=[dst, dport], initial_sequence=seq,
                       anchored=syn, timestamp=timestamp)
        stream = self.streams[sk]
        if syn and stream.initial is None and stream.received == 0:
            stream.initial = seq
            stream.expected = (seq + 1) & 0xffffffff
        stream.feed((seq + int(syn)) & 0xffffffff, data, timestamp)
        if flags & 1 and stream.expected is not None:
            stream.fin = stream.received + relative((seq + len(data)) & 0xffffffff, stream.expected)
        if flags & 4:
            stream.reset = True
        if flags & 16:
            peer = self.streams.get((conn, 's2c' if direction == 'c2s' else 'c2s'))
            if peer is not None and peer.expected is not None:
                bound = peer.received + relative(ack, peer.expected)
                if peer.fin is not None and bound == peer.fin + 1:
                    bound -= 1
                peer.acked = max(peer.acked, bound)
        if flags & 5:
            self.event(event='tcp-end', connection=conn, direction=direction,
                       timestamp=timestamp, flags=flags, sequence=seq, offset=stream.received,
                       fin_offset=stream.fin)

    def finish(self):
        for stream in self.streams.values():
            self.summaries.append(stream.finish())
        complete = not any(self.stats[k] for k in ('truncated_records', 'truncated_packets',
            'fragmented_ip_packets', 'reassembly_overflow', 'overlap_conflicts', 'unverified_overlap_bytes',
            'unanchored_packets', 'streams_with_gaps', 'partial_frames', 'unsupported_packets'))
        used = {s['connection'] for s in self.summaries if s['frames']}
        both = bool(used) and all(all(any(s['connection'] == c and s['direction'] == d and s['frames']
                    for s in self.summaries) for d in ('c2s', 's2c')) for c in used)
        closed = all(s['fin_offset'] is not None or s['reset'] for s in self.summaries if s['frames'])
        summary = dict(self.stats, complete_reassembly=bool(complete and both and closed),
                       observed_prefix_contiguous=bool(complete and both), both_directions_have_frames=both,
                       all_used_directions_closed=closed, semantic_proof=False, streams=self.summaries)
        self.log.close()
        (self.output / 'capture-summary.json').write_text(json.dumps(summary, indent=2) + '\n')


class Stream:
    def __init__(self, audit, conn, direction, initial, timestamp):
        self.audit, self.conn, self.direction = audit, conn, direction
        self.initial = initial
        self.expected = None if initial is None else (initial + 1) & 0xffffffff
        self.received = self.parsed = self.frames = 0
        self.pending = {}
        self.buffer = bytearray()
        self.history = collections.deque()
        self.history_bytes = 0
        self.previous = None
        self.failed = False
        self.timestamp = timestamp
        self.fin = None
        self.acked = 0
        self.reset = False

    def event(self, event, **fields):
        self.audit.event(event=event, connection=self.conn, direction=self.direction,
                         timestamp=self.timestamp, **fields)

    def compare(self, start, data, old_start, old):
        lo, hi = max(start, old_start), min(start + len(data), old_start + len(old))
        if hi > lo and data[lo - start:hi - start] != old[lo - old_start:hi - old_start]:
            self.event('overlap-conflict', offset=lo, length=hi - lo)
            self.audit.stats['overlap_conflicts'] += 1

    def feed(self, seq, data, timestamp):
        self.timestamp = timestamp
        if not data:
            return
        if self.expected is None:
            self.audit.stats['unanchored_packets'] += 1
            return
        start = self.received + relative(seq, self.expected)
        self.compare_history(start, data)
        if start < self.received - self.history_bytes:
            self.audit.stats['unverified_overlap_bytes'] += min(len(data), self.received - self.history_bytes - start)
        for at, old in self.pending.items():
            self.compare(start, data, at, old)
        if start > self.received:
            if sum(map(len, self.pending.values())) + len(data) > HISTORY:
                self.event('reassembly-overflow', offset=self.received, observed_offset=start)
                self.audit.stats['reassembly_overflow'] += 1
                self.expected = None
                self.pending.clear()
                return
            old = self.pending.get(start, b'')
            if len(data) > len(old):
                self.pending[start] = data
            self.audit.stats['out_of_order_packets'] += 1
            return
        skip = self.received - start
        self.audit.stats['overlapping_bytes'] += min(len(data), skip)
        if skip < len(data):
            self.accept(data[skip:])
        while self.pending:
            at = min(self.pending)
            if at > self.received:
                break
            data = self.pending.pop(at)
            self.compare_history(at, data)
            skip = self.received - at
            if skip < len(data):
                self.accept(data[skip:])

    def compare_history(self, start, data):
        if start >= self.received:
            return
        at = self.received - self.history_bytes
        for old in self.history:
            self.compare(start, data, at, old)
            at += len(old)
            if at >= start + len(data):
                break

    def accept(self, data):
        self.expected = (self.expected + len(data)) & 0xffffffff
        self.received += len(data)
        self.history.append(data)
        self.history_bytes += len(data)
        while self.history_bytes > HISTORY:
            self.history_bytes -= len(self.history.popleft())
        if self.failed:
            return
        self.buffer.extend(data)
        while len(self.buffer) >= 4:
            header = bytes(self.buffer[:4])
            size = int.from_bytes(header[1:], 'big')
            if header[0] != 0 or size < 4:
                self.failed = True
                self.event('invalid-frame', offset=self.parsed, header=header.hex(),
                           declared_length=size, previous=self.previous)
                self.audit.stats['invalid_frames'] += 1
                self.buffer.clear()
                return
            if len(self.buffer) < size + 4:
                break
            frame = bytes(self.buffer[4:size + 4])
            del self.buffer[:size + 4]
            fields = dict(offset=self.parsed, header=header.hex(), declared_length=size,
                          previous_declared_length=self.previous and self.previous['length'])
            self.event('frame', **fields, **self.metadata(frame))
            self.previous = dict(offset=self.parsed, length=size, header=header.hex())
            self.parsed += size + 4
            self.frames += 1
            self.audit.stats['frames'] += 1

    def metadata(self, frame):
        # Fixed identifiers only; unknown magic is classified, not dumped.
        magic = frame[:4]
        fields = dict(protocol={b'\xfeSMB': 'SMB2', b'\xffSMB': 'SMB1', b'\xfdSMB': 'encrypted',
                                b'\xfcSMB': 'compressed'}.get(magic, 'unknown'))
        if magic != b'\xfeSMB':
            if magic not in (b'\xffSMB', b'\xfdSMB', b'\xfcSMB'):
                self.audit.stats['unknown_protocol'] += 1
            return fields
        parts, errors = [], []
        at = 0
        while True:
            if at + 64 > len(frame) or frame[at:at + 4] != b'\xfeSMB':
                errors.append('invalid-compound-header')
                break
            header = frame[at:at + 64]
            command, flags, nxt = u16(header, 12), u32(header, 16), u32(header, 20)
            length = nxt if nxt else len(frame) - at
            part = dict(offset=at, length=length, structure_size=u16(header, 4), command=command,
                        credit_charge=u16(header, 6), credits=u16(header, 14), flags=flags,
                        message_id=u64(header, 24), status_or_channel=u32(header, 8), next_command=nxt)
            if length < 64 or at + length > len(frame) or (nxt and nxt % 8):
                errors.append('invalid-compound-length')
                parts.append(part)
                break
            body = frame[at + 64:at + length]
            if u16(header, 4) != 64:
                errors.append('invalid-header-structure-size')
            response = bool(flags & 1)
            if command in (8, 9) and not (response and u32(header, 8) != 0):
                if len(body) < (16 if response else (48 if command == 9 else 48)):
                    errors.append('short-read-write-body')
                elif command == 9 and response:
                    part.update(body_structure=u16(body, 0), write_count=u32(body, 4))
                elif command == 8 and not response:
                    part.update(body_structure=u16(body, 0), read_length=u32(body, 4), file_offset=u64(body, 8))
                else:
                    offset = body[2] if response else u16(body, 2)
                    count = u32(body, 4)
                    part.update(body_structure=u16(body, 0), data_offset=offset, data_length=count)
                    if command == 9 and not response:
                        part['file_offset'] = u64(body, 8)
                    if offset + count > length or (count and offset < 64 + (16 if response else 48)):
                        errors.append('invalid-data-range')
            parts.append(part)
            if not nxt:
                break
            at += nxt
        fields.update(parts=parts, validation_errors=errors)
        if errors:
            self.audit.stats['frames_with_validation_errors'] += 1
        # Baseline export deliberately omits payload digests: an untrusted
        # declared length can swallow a following authentication frame.
        return fields

    def finish(self):
        if self.fin is not None and self.acked == self.fin + 1:
            self.acked -= 1  # FIN can be captured after the ACK that covers it.
        terminal_gap = max(self.acked, self.fin or 0) > self.received
        partial = bool(self.buffer)
        if self.pending or terminal_gap:
            self.audit.stats['streams_with_gaps'] += 1
        if partial:
            self.audit.stats['partial_frames'] += 1
        summary = dict(connection=self.conn, direction=self.direction, anchored=self.initial is not None,
                       received=self.received, parsed=self.parsed, frames=self.frames, failed=self.failed,
                       pending_segments=len(self.pending), pending_bytes=sum(map(len, self.pending.values())),
                       partial_frame_bytes=len(self.buffer), next_sequence=self.expected,
                       fin_offset=self.fin, acknowledged_offset=self.acked, terminal_gap=terminal_gap, reset=self.reset)
        self.event('stream-summary', **{k: v for k, v in summary.items() if k not in ('connection', 'direction')})
        return summary


def audit_pcap(source, destination):
    audit = Audit(destination)
    with open(source, 'rb') as f:
        header = f.read(24)
        orders = {b'\xd4\xc3\xb2\xa1': ('<', 1e6), b'\xa1\xb2\xc3\xd4': ('>', 1e6),
                  b'\x4d\x3c\xb2\xa1': ('<', 1e9), b'\xa1\xb2\x3c\x4d': ('>', 1e9)}
        order, divisor = orders[header[:4]]
        link = struct.unpack(order + 'I', header[20:24])[0]
        audit.event(event='pcap', link_type=link, snaplen=struct.unpack(order + 'I', header[16:20])[0], export='metadata-only')
        if link not in (0, 1, 12, 108):
            raise ValueError(f'unsupported pcap link type {link}')
        while True:
            h = f.read(16)
            if not h:
                break
            if len(h) != 16:
                audit.stats['truncated_records'] += 1
                break
            sec, sub, incl, orig = struct.unpack(order + 'IIII', h)
            if incl > 2**20:
                raise ValueError('pcap record resource bound exceeded')
            packet = f.read(incl)
            audit.stats['packets'] += 1
            if incl != orig or len(packet) != incl:
                audit.stats['truncated_packets'] += 1
            at = {0: 4, 108: 4, 1: 14, 12: 0}[link]
            if len(packet) < at + 20 or packet[at] >> 4 != 4:
                audit.stats['unsupported_packets'] += 1
                continue
            ihl = (packet[at] & 15) * 4
            if packet[at + 9] != 6:
                continue
            if int.from_bytes(packet[at + 6:at + 8], 'big') & 0x3fff:
                audit.stats['fragmented_ip_packets'] += 1
                continue
            ip_end = at + int.from_bytes(packet[at + 2:at + 4], 'big')
            src, dst = socket.inet_ntoa(packet[at + 12:at + 16]), socket.inet_ntoa(packet[at + 16:at + 20])
            tcp = at + ihl
            if len(packet) < ip_end or len(packet) < tcp + 20:
                audit.stats['truncated_packets'] += 1
                continue
            sport, dport, seq, ack = struct.unpack('!HHII', packet[tcp:tcp + 12])
            start = tcp + (packet[tcp + 12] >> 4) * 4
            if ihl < 20 or start < tcp + 20 or start > ip_end:
                audit.stats['unsupported_packets'] += 1
                continue
            audit.tcp(src, dst, sport, dport, seq, ack, packet[tcp + 13], packet[start:ip_end], sec + sub / divisor)
    audit.finish()


if __name__ == '__main__':
    audit_pcap(sys.argv[1], sys.argv[2])
