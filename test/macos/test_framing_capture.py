# SPDX-License-Identifier: AGPL-3.0-only
import gzip
import json
from pathlib import Path
import socket
import struct
import tempfile
import unittest

from framing_capture import audit_pcap


def frame(command=9, payload=b'pattern', signature=b'S' * 16):
    body = bytearray(64)
    body[:4] = b'\xfeSMB'
    body[4:6] = (64).to_bytes(2, 'little')
    body[12:14] = command.to_bytes(2, 'little')
    body[40:48] = (1).to_bytes(8, 'little')
    body[48:64] = signature
    body.extend(payload)
    return len(body).to_bytes(4, 'big') + body


def packet(seq, flags, data=b'', reverse=False):
    ip = bytearray(20)
    ip[0] = 0x45
    ip[2:4] = (40 + len(data)).to_bytes(2, 'big')
    ip[9] = 6
    ip[12:16] = socket.inet_aton('127.0.0.1')
    ip[16:20] = socket.inet_aton('127.0.0.1')
    tcp = bytearray(20)
    tcp[:8] = struct.pack('!HHI', *( (1445, 55555) if reverse else (55555, 1445)), seq)
    tcp[12] = 0x50
    tcp[13] = flags
    return struct.pack('<I', 2) + ip + tcp + data


class CaptureTest(unittest.TestCase):
    def audit(self, packets):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        with (root / 'in.pcap').open('wb') as f:
            f.write(struct.pack('<IHHIIII', 0xa1b2c3d4, 2, 4, 0, 0, 262144, 0))
            for i, p in enumerate(packets):
                f.write(struct.pack('<IIII', 1700000000 + i, 0, len(p), len(p)))
                f.write(p)
        audit_pcap(root / 'in.pcap', root / 'out')
        events = [json.loads(l) for l in (root / 'out/frames.jsonl').read_text().splitlines()]
        return root / 'out', events

    def test_split_coalesced_out_of_order_retransmission_and_zero_header(self):
        auth = frame(1, b'NTLMSSP\x00DO-NOT-EXPORT')
        write = frame(9, b'Q' * 100000)
        data = auth + write + b'\x00\x00\x00\x00' + b'following'
        packets = [packet(100, 2), packet(501, 24, data[400:10000]),
                   packet(101, 24, data[:400]), packet(101, 24, data[:100]),
                   packet(10101, 24, data[10000:60000]), packet(60101, 24, data[60000:])]
        out, events = self.audit(packets)
        bad = [e for e in events if e['event'] == 'invalid-frame']
        self.assertEqual(len(bad), 1)
        self.assertEqual(bad[0]['offset'], len(auth) + len(write))
        self.assertEqual(bad[0]['header'], '00000000')
        self.assertEqual(bad[0]['previous']['length'], len(write) - 4)
        summary = [e for e in events if e['event'] == 'stream-summary'][0]
        tail = gzip.decompress((out / summary['tail_file']).read_bytes())
        self.assertNotIn(b'NTLMSSP', tail)
        self.assertNotIn(b'DO-NOT-EXPORT', tail)
        self.assertNotIn(b'S' * 16, tail)
        self.assertIn(b'following', tail)
        self.assertEqual(summary['pending_segments'], 0)

    def test_bidirectional_wraparound_and_missing_bytes(self):
        data = frame(9)
        out, events = self.audit([packet(0xfffffff0, 2), packet(1000, 18, reverse=True),
                                 packet(0xfffffff1, 24, data[:30]), packet(15, 24, data[30:]),
                                 packet(1001, 24, data, reverse=True),
                                 packet((0xfffffff1 + len(data) + 5) & 0xffffffff, 24, data)])
        frames = [e for e in events if e['event'] == 'frame']
        self.assertEqual({f['direction'] for f in frames}, {'c2s', 's2c'})
        self.assertEqual(len(frames), 2)
        summary = json.loads((out / 'capture-summary.json').read_text())
        self.assertEqual(summary['streams_with_gaps'], 1)

    def test_unanchored_never_invents_boundary(self):
        _, events = self.audit([packet(100, 24, frame())])
        self.assertFalse(any(e['event'] == 'frame' for e in events))
        self.assertFalse([e for e in events if e['event'] == 'stream-summary'][0]['anchored'])

    def test_invalid_header_after_auth_is_withheld_if_ntlm_marker(self):
        out, events = self.audit([packet(100, 2), packet(101, 24, frame() + b'\x85\x00\x00\x00NTLMSSP')])
        summary = [e for e in events if e['event'] == 'stream-summary'][0]
        self.assertTrue(summary['withheld_auth_marker'])
        self.assertFalse(list(out.glob('*.gz')))


if __name__ == '__main__':
    unittest.main()
