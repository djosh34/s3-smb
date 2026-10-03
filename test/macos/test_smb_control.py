# SPDX-License-Identifier: AGPL-3.0-only
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

from publish_diagnostic import publish
from smb_control import Control, verify
from smb_control_measure import measure
from smb_control_write import run


class MeasurementTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / 'framing').mkdir()
        self.summary = dict(observed_prefix_contiguous=True, complete_reassembly=True)
        self.state = dict(capture_alive_before_stop=True, tcpdump_exit=0, parser_exit=0,
                          health_ok=True, workload_ok=True, unmount_ok=True, write_start=0, write_end=20)
        (self.root / 'tcpdump-stderr.log').write_text('20 packets captured\n0 packets dropped by kernel\n')
        self.rows = []
        for i in range(8):
            self.rows.extend([
                dict(event='frame', connection='conn1', direction='c2s', protocol='SMB2', timestamp=i,
                     parts=[dict(command=9, message_id=i, flags=8, data_length=2**20)]),
                dict(event='frame', connection='conn1', direction='s2c', protocol='SMB2', timestamp=i+.1,
                     parts=[dict(command=9, message_id=i, flags=9, status_or_channel=0, write_count=2**20)])])

    def result(self):
        (self.root / 'framing/capture-summary.json').write_text(json.dumps(self.summary))
        (self.root / 'framing/frames.jsonl').write_text(''.join(json.dumps(row) + '\n' for row in self.rows))
        return measure(self.root, self.state, 8 * 2**20, 6, 2**20)

    def test_success_is_separate_from_historical_equivalence(self):
        result = self.result()
        self.assertTrue(result['clean_control'])
        self.assertEqual(result['wire_write_sizes'], {2**20: 8})
        self.assertIn('not proven historical', result['caveat'])

    def test_empty_or_syn_only_is_not_workload(self):
        self.rows = []
        self.assertFalse(self.result()['clean_control'])

    def test_partial_prefix_is_not_complete_capture(self):
        self.summary['complete_reassembly'] = False
        result = self.result()
        self.assertTrue(result['observed_prefix_valid'])
        self.assertFalse(result['clean_control'])

    def test_capture_failures_are_not_clean_negatives(self):
        for field, bad in [('capture_alive_before_stop', False), ('tcpdump_exit', 7),
                           ('parser_exit', 1), ('health_ok', False), ('unmount_ok', False)]:
            before = self.state[field]
            self.state[field] = bad
            self.assertFalse(self.result()['clean_control'], field)
            self.state[field] = before
        for text in ('no drop counter', '17 packets dropped by kernel'):
            (self.root / 'tcpdump-stderr.log').write_text(text)
            self.assertFalse(self.result()['clean_control'])

    def test_gap_and_parser_anomalies_fail(self):
        for key, value in [('observed_prefix_contiguous', False), ('invalid_frames', 1),
                           ('unknown_protocol', 1), ('frames_with_validation_errors', 1)]:
            self.summary[key] = value
            self.assertFalse(self.result()['clean_control'], key)
            self.summary = dict(observed_prefix_contiguous=True, complete_reassembly=True)

    def test_write_response_count_mismatch(self):
        self.rows[-1]['parts'][0]['write_count'] -= 1
        self.assertEqual(self.result()['write_requests_without_matching_success_count'], 1)
        self.assertFalse(self.result()['clean_control'])

    def test_bytes_duration_size_and_transforms_gate_exposure(self):
        original = json.dumps(self.rows)
        self.rows = self.rows[:-2]
        self.assertFalse(self.result()['exposure_target_met'])
        self.rows = json.loads(original)
        for row in self.rows:
            row['timestamp'] = 0
        self.assertFalse(self.result()['exposure_target_met'])
        self.rows = json.loads(original)
        self.rows[0]['protocol'] = 'encrypted'
        self.assertFalse(self.result()['exposure_target_met'])

    def test_reset_during_workload_prevents_clean_result(self):
        self.rows.append(dict(event='tcp-end', flags=4, timestamp=3))
        self.assertEqual(self.result()['resets_during_workload'], 1)
        self.assertFalse(self.result()['clean_control'])

    def test_publication_never_copies_untrusted_header_or_private_output(self):
        marker = 'SYNTHETIC_PRIVATE_MARKER'
        self.rows.append(dict(event='invalid-frame', header=marker.encode().hex(),
                              declared_length=123, previous=dict(header=marker.encode().hex())))
        self.result()
        (self.root / 'commands-private.log').write_text(marker)
        (self.root / 'private-traffic.pcap').write_text(marker)
        public = self.root / 'public'
        publish(self.root, public)
        outputs = b''.join(p.read_bytes() for p in public.rglob('*') if p.is_file())
        self.assertNotIn(marker.encode(), outputs)
        self.assertNotIn(marker.encode().hex().encode(), outputs)
        self.assertNotIn(b'"declared_length":123', outputs)


class WorkloadTest(unittest.TestCase):
    def test_local_fixture_and_independent_local_verifier(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                files = run(root / 'load', total_mib=8, seconds=0, workers=4, slot_mib=1)
            self.assertEqual(len(files), 4)
            (root / 'write.jsonl').write_text(output.getvalue())
            with contextlib.redirect_stdout(io.StringIO()):
                verify(root / 'write.jsonl', root / 'load')
            (root / 'load/writer-0.bin').write_bytes(b'wrong')
            with self.assertRaises(RuntimeError):
                verify(root / 'write.jsonl', root / 'load')

    def test_overlap_paths_are_rejected_before_any_mac_operations(self):
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(ValueError):
                Control(SimpleNamespace(work=directory, public=directory + '/public'))

    def test_bad_size_is_rejected(self):
        with self.assertRaises(ValueError):
            run('/must-not-create', total_mib=7, seconds=0, workers=4, slot_mib=1)


if __name__ == '__main__':
    unittest.main()
