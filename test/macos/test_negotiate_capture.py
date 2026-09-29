#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded header diagnostic tests; packet fixtures are not Apple evidence."""
import json
from pathlib import Path
import shutil
import signal
import struct
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from native import NEGOTIATE_FILTER, negotiate_header_evidence


class NegotiateCapture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)

    def fake_capture(self, argv, stdout, **kwargs):
        self.assertEqual(argv, ['/usr/sbin/tcpdump', '-i', 'lo0', '-y', 'NULL', '-n', '-s', '80', '-c', '4', '-XX', '-l', NEGOTIATE_FILTER])
        stdout.write(b'tcpdump: listening on lo0, link-type NULL (BSD loopback), snapshot length 80 bytes\n')
        process = Mock()
        process.poll.return_value = None
        process.wait.return_value = 0
        self.process = process
        return process

    def test_stopped_and_reaped_after_mount_failure(self):
        with patch('native.subprocess.Popen', side_effect=self.fake_capture):
            with self.assertRaisesRegex(RuntimeError, 'mount failed'):
                with negotiate_header_evidence(self.base):
                    raise RuntimeError('mount failed')
        self.process.send_signal.assert_called_once_with(signal.SIGINT)
        self.process.wait.assert_called_once_with(timeout=5)
        status = json.loads((self.base / 'smb-negotiate-header-status.json').read_text())
        self.assertEqual(status['exit'], 0)
        self.assertFalse(status['forced'])

    def test_stalled_capture_is_killed_reaped_and_fails(self):
        def capture(*args, **kwargs):
            process = self.fake_capture(*args, **kwargs)
            process.wait.side_effect = [subprocess.TimeoutExpired('tcpdump', 5), -9]
            return process
        with patch('native.subprocess.Popen', side_effect=capture):
            with self.assertRaisesRegex(RuntimeError, 'required kill'):
                with negotiate_header_evidence(self.base):
                    pass
        self.process.kill.assert_called_once()
        self.assertEqual(self.process.wait.call_count, 2)

    @unittest.skipUnless(shutil.which('tcpdump'), 'tcpdump required for executable BPF test')
    def test_filter_accepts_only_negotiate_and_headers_exclude_bodies(self):
        packets = []
        for port, magic, command in ((11101, b'\xffSMB', 0x72), (11102, b'\xffSMB', 0x73),
                                     (11103, b'\xfeSMB', 0), (11104, b'\xfeSMB', 1)):
            header = bytearray(32 if magic[0] == 0xff else 64)
            header[:4] = magic
            if magic[0] == 0xff:
                header[4] = command
            else:
                struct.pack_into('<H', header, 4, 64)
                struct.pack_into('<H', header, 12, command)
            body = bytes(header) + b'DO-NOT-CAPTURE-AUTH-OR-BODY'
            payload = len(body).to_bytes(4, 'big') + body
            ip = struct.pack('!BBHHHBBH4s4s', 0x45, 0, 40 + len(payload), 1, 0, 64, 6, 0,
                             b'\x7f\0\0\1', b'\x7f\0\0\1')
            tcp = struct.pack('!HHIIBBHHH', port, 445, 1, 0, 0x50, 0x18, 65535, 0, 0)
            # DLT_NULL matches Darwin lo0; header is native-endian AF_INET=2.
            packet = struct.pack('<I', 2) + ip + tcp + payload
            packets.append(packet)
        pcap = self.base / 'headers.pcap'
        with pcap.open('wb') as output:
            output.write(struct.pack('<IHHIIII', 0xa1b2c3d4, 2, 4, 0, 0, 80, 0))
            for packet in packets:
                captured = packet[:80]
                output.write(struct.pack('<IIII', 1, 0, len(captured), len(packet)))
                output.write(captured)
        result = subprocess.run([shutil.which('tcpdump'), '-nn', '-XX', '-r', str(pcap), NEGOTIATE_FILTER],
                                capture_output=True, text=True, check=True)
        self.assertIn('127.0.0.1.11101', result.stdout)
        self.assertIn('127.0.0.1.11103', result.stdout)
        self.assertNotIn('127.0.0.1.11102', result.stdout)
        self.assertNotIn('127.0.0.1.11104', result.stdout)
        self.assertNotIn('DO-NOT', result.stdout)
        self.assertIn('link-type NULL', result.stderr)


if __name__ == '__main__':
    unittest.main()
