import importlib.util
from pathlib import Path
import struct
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('window_audit', Path(__file__).with_name('window_audit.py'))
w = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = w
spec.loader.exec_module(w)


def packet(t, server=False, window=100, flags=16, seq=100, ack=200, length=0, scale=None):
    return w.Packet(int(t * w.NS), b'\x7f\0\0\1', b'\x7f\0\0\1',
                    1445 if server else 50000, 50000 if server else 1445,
                    seq, ack, flags, window, length, scale,
                    struct.pack('!IIHH', seq, ack, window, flags))


class Windows(unittest.TestCase):
    def audit(self, rows):
        a = w.Audit(1445)
        for p in rows:
            a.add(p)
        return a.output()[0]

    def handshake(self):
        return [packet(0, flags=2, scale=5, window=65000),
                packet(.1, server=True, flags=18, seq=199, ack=101, scale=4, window=65535)]

    def test_advertiser_not_recipient(self):
        s = self.audit(self.handshake() + [packet(1, server=True, window=0), packet(2, window=50)])['flows'][0]
        self.assertEqual(s['server_advertised']['zero_window_samples'], 1)
        self.assertEqual(s['client_advertised']['zero_window_samples'], 0)
        self.assertEqual(s['client_advertised']['scaled_window_sample_quantiles']['max'], 50 << 5)
        self.assertEqual(s['server_advertised']['negotiated_scale'], 4)

    def test_syn_and_rst_do_not_establish_zero_window(self):
        s = self.audit([packet(0, flags=2, window=0), packet(1, flags=20, window=0)])['flows'][0]['client_advertised']
        self.assertEqual(s['window_samples'], 0)
        self.assertEqual(s['counts']['rst'], 1)

    def test_unknown_scaling_without_syn(self):
        s = self.audit([packet(1, window=10)])['flows'][0]['client_advertised']
        self.assertIsNone(s['negotiated_scale'])
        self.assertIsNone(s['scaled_window_sample_quantiles']['max'])

    def test_scale_requires_both_offers(self):
        h = self.handshake()
        h[1].scale = None
        s = self.audit(h + [packet(1, window=10)])['flows'][0]['client_advertised']
        self.assertEqual(s['negotiated_scale'], 0)
        self.assertEqual(s['scaled_window_sample_quantiles']['max'], 10)

    def test_span_is_not_stall(self):
        s = self.audit([packet(0, window=1), packet(1, window=0),
                        packet(7, window=0, ack=250), packet(8, window=1)])['flows'][0]['client_advertised']
        e = s['top_zero_observation_runs'][0]
        self.assertEqual(e['zero_sample_span_ns'], 6 * w.NS)
        self.assertEqual(e['max_zero_intersample_gap_ns'], 6 * w.NS)
        self.assertEqual(e['bracketing_envelope_ns'], 8 * w.NS)
        self.assertEqual(e['continuous_duration_lower_bound_ns'], 0)
        self.assertEqual(e['ack_forward_between_zero_samples'], 50)

    def test_both_censoring_sides(self):
        e = self.audit([packet(1, window=0), packet(2, window=0)])['flows'][0]['client_advertised']['top_zero_observation_runs'][0]
        self.assertTrue(e['left_censored'])
        self.assertTrue(e['right_censored'])
        self.assertIsNone(e['bracketing_envelope_ns'])

    def test_wrap_and_reordered_ack(self):
        s = self.audit([packet(0, ack=w.MOD - 10), packet(1, ack=10), packet(2, ack=9)])['flows'][0]['client_advertised']
        self.assertEqual(s['counts']['ack_forward_bytes_minimum'], 20)
        self.assertEqual(s['counts']['backward_ack_observations'], 1)

    def test_possible_duplicate_not_discarded(self):
        s = self.audit([packet(1, window=0), packet(1.0001, window=0)])['flows'][0]['client_advertised']
        self.assertEqual(s['zero_window_samples'], 2)
        self.assertEqual(s['counts']['adjacent_equal_tcp_headers_within_1ms'], 1)

    def test_possible_probe_not_confirmed(self):
        s = self.audit([packet(0, server=True, window=0, ack=100), packet(1, seq=99, length=1)])['flows'][0]['client_advertised']
        self.assertEqual(s['counts']['probe_or_keepalive_candidates_after_peer_zero'], 1)

    def test_new_generation(self):
        s = self.audit([packet(0, flags=2, seq=100), packet(1, flags=2, seq=100), packet(2, flags=2, seq=900)])
        self.assertEqual(len(s['flows']), 2)
        self.assertEqual(s['flows'][1]['generation'], 2)

    def test_decode_valid_ipv4_syn_scale(self):
        ip = bytearray(20)
        ip[0], ip[9] = 0x45, 6
        struct.pack_into('!H', ip, 2, 44)
        ip[12:16] = ip[16:20] = b'\x7f\0\0\1'
        tcp = struct.pack('!HHIIBBHHH', 50000, 1445, 100, 0, 0x60, 2, 60000, 0, 0) + bytes([3, 3, 6, 0])
        h = struct.pack('<I', 2) + ip + tcp
        p = w.decode(h, len(h), 0, '<', 0)
        self.assertEqual((p.window, p.length, p.scale), (60000, 0, 6))

    def test_stream_rejects_partial_record(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / 'input'
            p.write_bytes(b'\xd4\xc3\xb2\xa1' + struct.pack('<HHIIII', 2, 4, 0, 0, 65535, 0) + b'\0')
            with self.assertRaises(w.MeasurementError):
                list(w.packets(p, w.Counter()))


if __name__ == '__main__':
    unittest.main()
