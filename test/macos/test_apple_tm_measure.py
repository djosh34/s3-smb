# SPDX-License-Identifier: AGPL-3.0-only
import json
from pathlib import Path
import plistlib
import tempfile
import unittest

from apple_tm_measure import (BAND_BYTES, GIB, PREFLIGHT, BACKING_CAP, RAW_CAP, CIPHER_CAP,
                              RESERVE, CAPTURE_BUFFER, budget_admitted, capacity, geometry, initial_result)


class GeometryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def bundle(self, band=BAND_BYTES):
        p = self.root / 'PRIVATE-NAME.sparsebundle'
        p.mkdir()
        (p / 'Info.plist').write_bytes(plistlib.dumps({'band-size': band, 'size': 16000000000000,
                                                       'private': 'PRIVATE-SENTINEL'}))
        (p / 'bands').mkdir()
        (p / 'bands' / 'PRIVATE-BAND').write_bytes(b'fixture')
        return p

    def test_empty_and_ambiguous(self):
        self.assertEqual(geometry(self.root)['parse'], 'absent')
        self.bundle()
        (self.root / 'other.sparsebundle').mkdir()
        self.assertEqual(geometry(self.root)['parse'], 'ambiguous')

    def test_geometry_is_not_write_or_exposure_proof(self):
        self.bundle()
        g = geometry(self.root)
        self.assertTrue(g['target_band_geometry'])
        self.assertEqual(g['largest_band_bytes'], 7)
        self.assertEqual(g['bands_over_4gib'], 0)
        self.assertNotIn('PRIVATE', json.dumps(g))
        r = initial_result()
        self.assertEqual(r['pairwise_exposure_match'], 'unknown')
        self.assertFalse(r['capture_completeness_proven'])
        self.assertEqual(r['restore'], 'not-run')
        self.assertEqual(r['recovery'], 'not-run')
        self.assertIsNone(r['short_header_count'])

    def test_wrong_band_is_measured_not_rewritten(self):
        p = self.bundle(8 << 20)
        before = (p / 'Info.plist').read_bytes()
        self.assertFalse(geometry(self.root)['target_band_geometry'])
        self.assertEqual(before, (p / 'Info.plist').read_bytes())

    def test_actual_large_band_file(self):
        p = self.bundle()
        with (p / 'bands' / 'PRIVATE-BAND').open('r+b') as f:
            f.truncate(5 * GIB)  # Sparse metadata only; no bulk test write.
        g = geometry(self.root)
        self.assertEqual(g['bands_over_4gib'], 1)
        self.assertLess(g['band_allocated_bytes'], g['band_logical_bytes'])

    def test_untrusted_plist_types(self):
        p = self.bundle()
        for value in [True, '8589934592', 0, -1, 1 << 63]:
            (p / 'Info.plist').write_bytes(plistlib.dumps({'band-size': value, 'size': 1}))
            self.assertEqual(geometry(self.root)['parse'], 'invalid')

    def test_symlink_not_followed(self):
        p = self.bundle()
        (p / 'bands' / 'link').symlink_to('/not-read')
        self.assertEqual(geometry(self.root)['parse'], 'invalid')

    def test_bad_plist_no_exception_text(self):
        p = self.bundle()
        (p / 'Info.plist').write_bytes(b'PRIVATE-SENTINEL')
        g = geometry(self.root)
        self.assertEqual(g['parse'], 'invalid')
        self.assertNotIn('PRIVATE', json.dumps(g))

    def test_partial_xml_observation_is_unavailable_not_workload_error(self):
        p = self.bundle()
        (p / 'Info.plist').write_bytes(b'<?xml version="1.0"?><plist><dict><key>band-size</key>')
        g = geometry(self.root)
        self.assertEqual(g['parse'], 'invalid')
        self.assertFalse(g['target_band_geometry'])

    def test_exact_admitted_budget_and_buffer_request(self):
        self.assertEqual(BACKING_CAP + RAW_CAP + GIB + CIPHER_CAP + RESERVE, 95 * GIB)
        self.assertEqual(PREFLIGHT, 96 * GIB)
        self.assertEqual(CIPHER_CAP + RESERVE, 46 * GIB)
        self.assertEqual(CAPTURE_BUFFER, 32 << 20)

    def test_budget_is_physical_not_virtual(self):
        self.assertTrue(budget_admitted(PREFLIGHT))
        self.assertFalse(budget_admitted(PREFLIGHT - 1))
        c = capacity(self.root)
        self.assertGreater(c['total_bytes'], 0)
        self.assertLessEqual(c['available_bytes'], c['total_bytes'])


if __name__ == '__main__':
    unittest.main()
