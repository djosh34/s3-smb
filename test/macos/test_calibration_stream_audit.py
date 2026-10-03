import contextlib
import io
import struct
import unittest

import calibration_stream_audit as a
from window_audit import Packet, MeasurementError


def p(server=False, seq=101, ack=501, flags=16, payload=b''):
    return Packet(0, bytes([127,0,0,1]), bytes([127,0,0,1]),
                  a.PORT if server else 50000, 50000 if server else a.PORT,
                  seq, ack, flags, 100, len(payload)), payload


class Calibration(unittest.TestCase):
    def setUp(self):
        self.old = a.EXPECTED
        a.EXPECTED = (4, 8)
        self.silent = contextlib.redirect_stdout(io.StringIO())
        self.silent.__enter__()

    def tearDown(self):
        self.silent.__exit__(None, None, None)
        a.EXPECTED = self.old

    def flow(self):
        f = a.Flow()
        f.observe(*p(seq=100, ack=0, flags=2))
        f.observe(*p(server=True, seq=500, ack=101, flags=18))
        f.observe(*p(seq=101, ack=501))
        return f

    def finish(self, f):
        f.observe(*p(seq=105, flags=17))
        f.observe(*p(server=True, seq=501, ack=106, flags=24, payload=struct.pack('!Q', a.TOTAL)))
        f.observe(*p(server=True, seq=509, ack=106, flags=17))
        f.observe(*p(seq=106, ack=510))
        return f.result()

    def test_union_late_fill_and_overlap(self):
        u = a.Union()
        self.assertEqual(u.add(4, 8), 0)
        self.assertEqual(u.add(0, 6), 2)
        self.assertEqual(u.ranges, [(0, 8)])
        self.assertEqual(u.add(2, 4), 2)
        self.assertEqual(u.unique, 8)

    def test_union_gap_exact(self):
        u = a.Union()
        u.add(0, 3)
        u.add(7, 9)
        self.assertEqual(u.gaps(10), [[3, 7], [9, 10]])

    def test_wrap_and_old_sequence(self):
        self.assertEqual(a.lift(4, a.MOD-4, 8), 8)
        self.assertEqual(a.lift(2, a.MOD-4, 10), 6)
        self.assertEqual(a.lift(100, 100, 2*a.MOD), 2*a.MOD)

    def test_complete_with_old_pure_ack(self):
        f = self.flow()
        f.observe(*p(payload=bytes(4)))
        f.observe(*p(seq=101))
        result = self.finish(f)
        self.assertTrue(result['complete'])
        error = result['original_c_first_rejection']
        self.assertEqual(error['reason'], 'exact_next_sequence_requirement')
        self.assertEqual(error['seq_minus_current_next'], -4)
        self.assertEqual(error['payload_length'], 0)

    def test_resegmented_duplicate_known_pattern(self):
        f = self.flow()
        f.observe(*p(payload=bytes(4)))
        f.observe(*p(seq=102, payload=bytes(2)))
        result = self.finish(f)
        self.assertTrue(result['complete'])
        self.assertEqual(result['directions'][0]['counts']['overlap_bytes'], 2)

    def test_reordered_payload_late_fill(self):
        f = self.flow()
        f.observe(*p(seq=103, payload=bytes(2)))
        f.observe(*p(seq=101, payload=bytes(2)))
        result = self.finish(f)
        self.assertTrue(result['complete'])
        self.assertEqual(result['directions'][0]['max_disjoint_ranges'], 1)

    def test_gap_not_hidden_by_fin_ack(self):
        f = self.flow()
        f.observe(*p(payload=bytes(2)))
        result = self.finish(f)
        self.assertFalse(result['complete'])
        self.assertEqual(result['directions'][0]['first_gaps'], [[2,4]])

    def test_conflicting_payload_pattern(self):
        f = self.flow()
        f.observe(*p(payload=bytes(4)))
        f.observe(*p(seq=102, payload=bytes([1])))
        self.assertFalse(self.finish(f)['complete'])

    def test_terminal_ack_required(self):
        f = self.flow()
        f.observe(*p(payload=bytes(4)))
        f.observe(*p(seq=105, flags=17))
        self.assertFalse(f.result()['complete'])

    def test_ack_regression_does_not_erase_high(self):
        f = self.flow()
        f.observe(*p(payload=bytes(4)))
        f.observe(*p(server=True, seq=501, ack=105))
        f.observe(*p(server=True, seq=501, ack=103))
        result = self.finish(f)
        self.assertTrue(result['complete'])
        self.assertEqual(result['original_c_first_rejection']['reason'], 'ack_regression')


if __name__ == '__main__':
    unittest.main()
