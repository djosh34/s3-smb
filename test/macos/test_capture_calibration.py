# SPDX-License-Identifier: AGPL-3.0-only
import json
import os
from pathlib import Path
import struct
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

import capture_calibration as calibration


def packet(seq, ack, flags, payload=b'', reverse=False):
    src, dst = (28464, 50000) if reverse else (50000, 28464)
    tcp = struct.pack('!HHIIBBHHH', src, dst, seq & 0xffffffff, ack & 0xffffffff,
                      0x50, flags, 65535, 0, 0) + payload
    ip = struct.pack('!BBHHHBBHII', 0x45, 0, 20+len(tcp), 0, 0, 64, 6, 0,
                     0x7f000001, 0x7f000001)
    return struct.pack('<I', 2) + ip + tcp


def flow(base=100):
    return [packet(base, 0, 2), packet(200, base+1, 18, reverse=True),
            packet(base+1, 201, 16), packet(base+1, 201, 24, b'x'*32),
            packet(base+33, 201, 17), packet(201, base+34, 24, b'y'*8, True),
            packet(209, base+34, 17, reverse=True), packet(base+34, 210, 16)]


def pcap(packets):
    out = struct.pack('<IHHIIII', 0xa1b2c3d4, 2, 4, 0, 0, 262144, 0)
    for i, payload in enumerate(packets):
        out += struct.pack('<IIII', 100+i, 0, len(payload), len(payload)) + payload
    return out


class Coverage(unittest.TestCase):
    def audit(self, data, expected=True):
        binary = os.environ.get('CALIBRATION_AUDIT_BIN')
        if not binary:
            self.skipTest('compile native calibration_audit.c and set CALIBRATION_AUDIT_BIN')
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'fixture.pcap'
            path.write_bytes(data)
            result = subprocess.run([binary, str(path), '28464', '32', '8'], capture_output=True, timeout=5)
            self.assertEqual(result.returncode == 0, expected)
            if result.stdout:
                value = json.loads(result.stdout)
                self.assertEqual(value['complete'], expected)
                self.assertTrue(all(type(v) in (int, float, bool) for v in value.values()))

    def test_full_flow(self): self.audit(pcap(flow()))
    def test_sequence_wrap(self): self.audit(pcap(flow(0xfffffff0)))
    def test_missing_syn(self): self.audit(pcap(flow()[1:]), False)
    def test_missing_synack(self): self.audit(pcap(flow()[:1]+flow()[2:]), False)
    def test_missing_body(self): self.audit(pcap(flow()[:3]+flow()[4:]), False)
    def test_missing_terminal_ack(self): self.audit(pcap(flow()[:-1]), False)
    def test_unclosed(self): self.audit(pcap(flow()[:4]), False)
    def test_reordered(self):
        values = flow()
        values[3], values[4] = values[4], values[3]
        self.audit(pcap(values), False)
    def test_retransmission_conservative(self): self.audit(pcap(flow()[:4]+flow()[3:]), False)
    def test_truncated_pcap(self): self.audit(pcap(flow())[:-1], False)
    def test_snaplen_truncation(self):
        data = bytearray(pcap(flow()))
        struct.pack_into('<I', data, 24+12, 99)
        self.audit(data, False)
    def test_reset(self): self.audit(pcap(flow()[:4]+[packet(201, 134, 20, reverse=True)]), False)
    def test_bad_first_ack(self):
        values = flow()
        values[1] = packet(200, 100, 18, reverse=True)
        self.audit(pcap(values), False)
    def test_wrong_flow(self): self.audit(pcap(flow()+flow()), False)


class Publication(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root/'evidence/capture').mkdir(parents=True)
        self.public = self.root/'public'
        self.public.mkdir()
        (self.root/'encryption-success').write_bytes(b'complete\n')
        (self.root/'evidence/provenance.json').write_text(json.dumps(dict(
            helper_sha256='a'*64, checker_sha256='b'*64, harness_revision='c'*40,
            os_version='15.7.9', os_build='24G830')))
        (self.public/'capture.tar.gz.cms').write_bytes(b'synthetic ciphertext presence marker')
        self.values = {
            name: {field: (False if kind is bool else 0.0 if kind is float else 0)
                   for field, kind in schema.items()} for name, schema in calibration.SCHEMAS.items()}
        self.values['ready'].update(effective_buffer_bytes=33554432, requested_buffer_bytes=33554432,
                                    snaplen=262144, datalink=0)
        self.values['summary'].update(capture_health_valid=True, drained=True, packets=8, received=8,
                                      effective_buffer_bytes=33554432, requested_buffer_bytes=33554432)
        self.values['workload'].update(workload_valid=True, hashes_equal=True,
                                       sent_bytes=calibration.TOTAL, received_bytes=calibration.TOTAL, reply_bytes=8)
        self.values['audit'].update(complete=True, packets=8, client_bytes=calibration.TOTAL,
                                    server_bytes=8, client_payload_span_seconds=299.99)

    def publish(self):
        for name, value in self.values.items():
            parent = self.root/'evidence/capture' if name in ('ready', 'summary') else self.root/'evidence'
            (parent/(name+'.json')).write_text(json.dumps(value))
        return calibration.publish(self.root, self.public)

    def test_valid(self): self.assertTrue(self.publish())
    def test_drop_invalid(self):
        self.values['summary']['dropped'] = 1
        self.assertFalse(self.publish())
    def test_incomplete_invalid(self):
        self.values['audit']['complete'] = False
        self.assertFalse(self.publish())
    def test_wrong_count_invalid(self):
        self.values['audit']['client_bytes'] -= 1
        self.assertFalse(self.publish())
    def test_no_retention_invalid(self):
        (self.public/'capture.tar.gz.cms').unlink()
        self.assertFalse(self.publish())
    def test_failed_retention_after_cipher_move_invalid(self):
        (self.root/'encryption-success').unlink()
        self.assertFalse(self.publish())
    def test_private_string_not_exported(self):
        marker = 'PRIVATE_FIXTURE_DO_NOT_EXPORT'
        self.values['summary']['packets'] = marker
        self.assertFalse(self.publish())
        self.assertNotIn(marker, (self.public/'calibration.json').read_text())
    def test_extra_private_field_dropped(self):
        self.values['summary']['private'] = 'PRIVATE_FIXTURE_DO_NOT_EXPORT'
        self.assertTrue(self.publish())
        self.assertNotIn('PRIVATE_FIXTURE_DO_NOT_EXPORT', (self.public/'calibration.json').read_text())
    def test_nonfinite_rejected(self):
        self.values['audit']['client_payload_span_seconds'] = float('nan')
        self.assertFalse(self.publish())
    def test_bool_not_integer(self):
        self.values['summary']['packets'] = True
        self.assertFalse(self.publish())
    def test_retention_reservation(self):
        (self.root/'private-traffic.pcap').write_bytes(b'fixture')
        with patch.object(calibration.shutil, 'disk_usage', return_value=SimpleNamespace(free=20*1024**3)):
            self.assertTrue(calibration.retention_preflight(self.root))
    def test_retention_low_space(self):
        (self.root/'private-traffic.pcap').write_bytes(b'fixture')
        with patch.object(calibration.shutil, 'disk_usage', return_value=SimpleNamespace(free=4*1024**3)):
            self.assertFalse(calibration.retention_preflight(self.root))
    def test_retention_link_rejected(self):
        (self.root/'private-traffic.pcap').write_bytes(b'fixture')
        (self.root/'evidence/link').symlink_to(self.root/'private-traffic.pcap')
        with self.assertRaises(ValueError):
            calibration.retention_preflight(self.root)
    def test_tiny_ordinary_socket_hashes(self):
        result = calibration.transfer(1024*1024, 0, 0)
        self.assertTrue(result['workload_valid'])
        self.assertTrue(result['hashes_equal'])
        self.assertEqual(result['received_bytes'], 1024*1024)


if __name__ == '__main__':
    unittest.main()
