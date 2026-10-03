# SPDX-License-Identifier: AGPL-3.0-only
import json
from pathlib import Path
import socket
import struct
import tempfile
import unittest
from framing_capture import audit_pcap


def frame(command=13, payload=b'pattern'):
    body = bytearray(64)
    body[:4] = b'\xfeSMB'
    body[4:6] = (64).to_bytes(2, 'little')
    body[12:14] = command.to_bytes(2, 'little')
    body[40:48] = (1).to_bytes(8, 'little')
    body.extend(payload)
    return len(body).to_bytes(4, 'big') + body


def packet(seq, flags, data=b'', reverse=False, ack=0):
    ip = bytearray(20)
    ip[0] = 0x45
    ip[2:4] = (40 + len(data)).to_bytes(2, 'big')
    ip[9] = 6
    ip[12:16] = socket.inet_aton('127.0.0.1')
    ip[16:20] = socket.inet_aton('127.0.0.1')
    tcp = bytearray(20)
    tcp[:12] = struct.pack('!HHII', *((1445, 55555) if reverse else (55555, 1445)), seq, ack)
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
        summary = json.loads((root / 'out/capture-summary.json').read_text())
        return root / 'out', events, summary

    def test_split_coalesced_reorder_and_invalid(self):
        auth = frame(1, b'AUTH-MUST-NOT-EXPORT')
        write = frame(13, b'Q' * 100000)
        data = auth + write + b'\x00\x00\x00\x00SECRET'
        out, events, summary = self.audit([packet(100, 2), packet(501, 24, data[400:10000]),
            packet(101, 24, data[:400]), packet(101, 24, data[:100]),
            packet(10101, 24, data[10000:60000]), packet(60101, 24, data[60000:])])
        bad = [e for e in events if e['event'] == 'invalid-frame'][0]
        self.assertEqual(bad['offset'], len(auth) + len(write))
        self.assertEqual(bad['header'], '00000000')
        self.assertEqual(bad['previous']['length'], len(write) - 4)
        self.assertEqual(summary['streams'][0]['pending_segments'], 0)
        for path in out.iterdir():
            self.assertEqual(path.suffix, '.json' if path.name == 'capture-summary.json' else '.jsonl')
            self.assertNotIn(b'SECRET', path.read_bytes())
            self.assertNotIn(b'AUTH-MUST-NOT-EXPORT', path.read_bytes())

    def test_same_sequence_longer_pending_is_not_lost(self):
        data = frame(13, b'X' * 200)
        _, _, summary = self.audit([packet(100, 2), packet(121, 24, data[20:50]),
                                   packet(121, 24, data[20:]), packet(101, 24, data[:20])])
        self.assertEqual(summary['streams'][0]['received'], len(data))
        self.assertEqual(summary['frames'], 1)

    def test_conflicting_overlap_detected(self):
        data = frame()
        _, _, summary = self.audit([packet(100, 2), packet(101, 24, data), packet(101, 24, b'X' + data[1:])])
        self.assertEqual(summary['overlap_conflicts'], 1)
        self.assertFalse(summary['complete_reassembly'])

    def test_fin_proves_missing_tail(self):
        data = frame()
        _, _, summary = self.audit([packet(100, 2), packet(101, 24, data), packet(101 + len(data) + 100, 17)])
        self.assertEqual(summary['streams_with_gaps'], 1)
        self.assertTrue(summary['streams'][0]['terminal_gap'])

    def test_ack_proves_missing_tail(self):
        data = frame()
        _, _, summary = self.audit([packet(100, 2), packet(1000, 18, reverse=True, ack=101),
                packet(101, 24, data), packet(1001, 16, reverse=True, ack=101 + len(data) + 100)])
        self.assertEqual(summary['streams_with_gaps'], 1)

    def test_synack_before_syn_and_sequence_wrap(self):
        data = frame()
        _, _, summary = self.audit([packet(1000, 18, reverse=True, ack=0xfffffff1), packet(0xfffffff0, 2),
            packet(0xfffffff1, 24, data[:30]), packet(15, 24, data[30:]), packet(1001, 24, data, reverse=True)])
        self.assertEqual(summary['frames'], 2)
        self.assertTrue(summary['observed_prefix_contiguous'])
        self.assertFalse(summary['complete_reassembly'])

    def test_unanchored_and_partial_never_clean(self):
        _, _, summary = self.audit([packet(100, 24, frame())])
        self.assertFalse(summary['complete_reassembly'])
        _, _, summary = self.audit([packet(100, 2), packet(101, 24, frame()[:10])])
        self.assertEqual(summary['partial_frames'], 1)
        self.assertFalse(summary['complete_reassembly'])

    def test_enlarged_write_and_compound_never_export_bytes(self):
        auth = frame(1, b'AUTH-MUST-NOT-EXPORT')
        write = frame(9, b'P' * 48) + auth
        write = len(write[4:]).to_bytes(4, 'big') + write[4:]
        out, _, _ = self.audit([packet(100, 2), packet(101, 24, write)])
        for path in out.iterdir():
            self.assertNotIn(b'AUTH-MUST-NOT-EXPORT', path.read_bytes())


if __name__ == '__main__':
    unittest.main()
