#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Offline full-snaplen TCP framing audit; raw pcap stays private runner scratch.

Usage: framing_capture.py INPUT.pcap OUTPUT_DIRECTORY
Offsets count TCP application bytes from SYN+1 independently in each direction.
Exports metadata from connection start and 16MiB sanitized tails per direction.
An invalid frame freezes the preceding tail plus at most 4096 following bytes.
SESSION_SETUP bodies and SMB signatures are redacted, never printed. Unknown
pre-authentication bytes are not exported. No successful parse excuses a prior
server response defect; both directions must be inspected.
"""
import collections
import gzip
import json
from pathlib import Path
import socket
import struct
import sys

TAIL = 16 * 1024 * 1024
MAX_FRAME = 16 * 1024 * 1024


class Audit:
    def __init__(self, output):
        self.output = Path(output)
        self.output.mkdir(parents=True, exist_ok=True)
        self.log = (self.output / 'frames.jsonl').open('w')
        self.streams = {}
        self.generations = collections.Counter()
        self.connections = {}
        self.stats = collections.Counter()

    def event(self, **fields):
        self.log.write(json.dumps(fields, separators=(',', ':')) + '\n')

    def tcp(self, src, dst, sport, dport, seq, flags, data, timestamp):
        if 1445 not in (sport, dport) and 445 not in (sport, dport):
            return
        client, server = ((src, sport), (dst, dport)) if dport in (1445, 445) else ((dst, dport), (src, sport))
        key = (client, server)
        direction = 'c2s' if dport in (1445, 445) else 's2c'
        syn = bool(flags & 2)
        if syn and not flags & 16:
            old = self.connections.get(key)
            if old is None or self.streams[(old, 'c2s')].initial != seq:
                self.generations[key] += 1
                conn = f'conn{len(self.connections) + sum(self.generations.values()):04d}-{client[1]}'
                self.connections[key] = conn
        if key not in self.connections:
            self.connections[key] = f'unanchored{len(self.connections):04d}-{client[1]}'
        conn = self.connections[key]
        sk = (conn, direction)
        if sk not in self.streams:
            self.streams[sk] = Stream(self, conn, direction, seq if syn else None, timestamp)
            self.event(event='connection-direction', connection=conn, direction=direction,
                       source=[src, sport], destination=[dst, dport], initial_sequence=seq,
                       anchored=syn, timestamp=timestamp)
        stream = self.streams[sk]
        stream.feed((seq + int(syn)) & 0xffffffff, data, timestamp)
        if flags & 5:
            self.event(event='tcp-end', connection=conn, direction=direction,
                       timestamp=timestamp, flags=flags, sequence=seq, offset=stream.received)

    def finish(self):
        for stream in self.streams.values():
            stream.finish()
        self.log.close()
        (self.output / 'capture-summary.json').write_text(json.dumps(dict(self.stats), indent=2) + '\n')


class Stream:
    def __init__(self, audit, conn, direction, initial, timestamp):
        self.audit, self.conn, self.direction = audit, conn, direction
        self.initial = initial
        self.expected = None if initial is None else (initial + 1) & 0xffffffff
        self.received = 0
        self.parsed = 0
        self.pending = {}
        self.buffer = bytearray()
        self.tail = bytearray()
        self.previous = None
        self.failed = False
        self.failure_extra = 0
        self.authenticated = False
        self.timestamp = timestamp
        self.redactions = []
        self.frames = 0

    def event(self, event, **fields):
        self.audit.event(event=event, connection=self.conn, direction=self.direction,
                         timestamp=self.timestamp, **fields)

    def save(self, data):
        self.tail.extend(data)
        if len(self.tail) > TAIL:
            del self.tail[:len(self.tail) - TAIL]

    def feed(self, seq, data, timestamp):
        self.timestamp = timestamp
        if not data:
            return
        if self.expected is None:
            self.audit.stats['unanchored_packets'] += 1
            return
        delta = (seq - self.expected + 2**31) % 2**32 - 2**31
        if delta < 0:
            self.audit.stats['retransmitted_or_overlapping_bytes'] += min(len(data), -delta)
            data = data[-delta:]
            seq = self.expected
            delta = 0
        if not data:
            return
        if delta:
            # Keep bounded out-of-order segments, never silently resynchronize.
            if sum(map(len, self.pending.values())) + len(data) > 32 * 1024 * 1024:
                self.event('reassembly-overflow', next_sequence=self.expected, observed_sequence=seq)
                self.audit.stats['reassembly_overflow'] += 1
                self.expected = None
                self.pending.clear()
                return
            self.pending.setdefault(seq, data)
            self.audit.stats['out_of_order_packets'] += 1
            return
        self.accept(data)
        # A retransmission can bridge into a pending segment; process overlaps.
        while self.pending:
            candidates = [(s, d) for s, d in self.pending.items()
                          if (s - self.expected + 2**31) % 2**32 - 2**31 <= 0]
            if not candidates:
                break
            for s, d in candidates:
                del self.pending[s]
                skip = (self.expected - s) & 0xffffffff
                if skip < len(d):
                    self.accept(d[skip:])

    def accept(self, data):
        self.expected = (self.expected + len(data)) & 0xffffffff
        self.received += len(data)
        if self.failed:
            if self.authenticated and self.failure_extra < 4096:
                extra = data[:4096 - self.failure_extra]
                self.save(extra)
                self.failure_extra += len(extra)
            return
        self.buffer.extend(data)
        while len(self.buffer) >= 4:
            header = bytes(self.buffer[:4])
            size = int.from_bytes(header[1:], 'big')
            if header[0] != 0 or size < 4 or size > MAX_FRAME:
                self.failed = True
                self.event('invalid-frame', offset=self.parsed, header=header.hex(),
                           declared_length=size, previous=self.previous)
                self.audit.stats['invalid_frames'] += 1
                # No unauthenticated payload bytes go to artifacts.
                extra = bytes(self.buffer[:4096]) if self.authenticated else header
                self.save(extra)
                self.failure_extra = len(extra)
                self.buffer.clear()
                return
            if len(self.buffer) < size + 4:
                break
            frame = bytes(self.buffer[:size + 4])
            del self.buffer[:size + 4]
            magic = frame[4:8]
            fields = dict(offset=self.parsed, header=header.hex(), declared_length=size,
                          magic=magic.hex(), previous_declared_length=self.previous and self.previous['length'])
            safe = bytearray(frame)
            if magic == b'\xfeSMB' and size >= 64:
                command = int.from_bytes(frame[16:18], 'little')
                fields.update(command=command, message_id=int.from_bytes(frame[28:36], 'little'),
                              flags=int.from_bytes(frame[20:24], 'little'),
                              next_command=int.from_bytes(frame[24:28], 'little'),
                              status_or_channel=int.from_bytes(frame[12:16], 'little'))
                if command >= 2 and int.from_bytes(frame[44:52], 'little'):
                    self.authenticated = True
                safe[52:68] = bytes(16)  # signatures are not needed for framing
                if command in (0, 1):
                    safe[68:] = bytes(len(safe) - 68)
                    self.redactions.append([self.parsed + 68, self.parsed + len(safe)])
                # Compounds may include SESSION_SETUP; fail closed for exported bytes.
                nxt = fields['next_command']
                seen = set()
                while nxt and nxt not in seen and nxt + 68 <= len(frame):
                    seen.add(nxt)
                    at = 4 + nxt
                    if frame[at:at + 4] != b'\xfeSMB':
                        break
                    safe[at + 48:at + 64] = bytes(16)
                    if int.from_bytes(frame[at + 12:at + 14], 'little') in (0, 1):
                        safe[at + 64:] = bytes(len(safe) - at - 64)
                        self.redactions.append([self.parsed + at + 64, self.parsed + len(safe)])
                        break
                    delta = int.from_bytes(frame[at + 20:at + 24], 'little')
                    nxt = nxt + delta if delta else 0
            else:
                # SMB1 negotiation, encryption/compression transforms and unknown
                # protocols are not decoded here; retain lengths, not their bytes.
                safe[8:] = bytes(len(safe) - 8)
                self.redactions.append([self.parsed + 8, self.parsed + len(safe)])
            self.event('frame', **fields)
            self.previous = dict(offset=self.parsed, length=size, header=header.hex())
            self.save(safe)
            self.parsed += len(frame)
            self.frames += 1
            self.audit.stats['frames'] += 1

    def finish(self):
        end = self.parsed + self.failure_extra
        # A marker anywhere in the bounded tail makes export fail closed. Raw
        # scratch is deliberately not an artifact, even when parsing fails.
        withheld = b'NTLMSSP' in self.tail
        filename = f'{self.conn}-{self.direction}.stream.gz'
        if not withheld:
            with gzip.open(self.audit.output / filename, 'wb', compresslevel=1) as f:
                f.write(self.tail)
        self.event('stream-summary', anchored=self.initial is not None, received=self.received,
                   parsed=self.parsed, frames=self.frames, failed=self.failed,
                   pending_segments=len(self.pending), pending_bytes=sum(map(len, self.pending.values())),
                   partial_frame_bytes=len(self.buffer), next_sequence=self.expected,
                   tail_start=end - len(self.tail), tail_end=end, redactions=self.redactions,
                   tail_file=None if withheld else filename, withheld_auth_marker=withheld)
        if self.pending:
            self.audit.stats['streams_with_gaps'] += 1


def audit_pcap(source, destination):
    audit = Audit(destination)
    with open(source, 'rb') as f:
        header = f.read(24)
        orders = {b'\xd4\xc3\xb2\xa1': ('<', 1e6), b'\xa1\xb2\xc3\xd4': ('>', 1e6),
                  b'\x4d\x3c\xb2\xa1': ('<', 1e9), b'\xa1\xb2\x3c\x4d': ('>', 1e9)}
        order, divisor = orders[header[:4]]
        link = struct.unpack(order + 'I', header[20:24])[0]
        audit.event(event='pcap', link_type=link, snaplen=struct.unpack(order + 'I', header[16:20])[0])
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
            packet = f.read(incl)
            audit.stats['packets'] += 1
            if incl != orig or len(packet) != incl:
                audit.stats['truncated_packets'] += 1
            at = {0: 4, 108: 4, 1: 14, 12: 0}[link]
            if len(packet) < at + 20 or packet[at] >> 4 != 4:
                audit.stats['non_ipv4_packets'] += 1
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
            if len(packet) < tcp + 20:
                continue
            sport, dport, seq = struct.unpack('!HHI', packet[tcp:tcp + 8])
            data = packet[tcp + (packet[tcp + 12] >> 4) * 4:ip_end]
            audit.tcp(src, dst, sport, dport, seq, packet[tcp + 13], data, sec + sub / divisor)
    audit.finish()


if __name__ == '__main__':
    audit_pcap(sys.argv[1], sys.argv[2])
