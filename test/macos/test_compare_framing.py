# SPDX-License-Identifier: AGPL-3.0-only
import json
from pathlib import Path
import tempfile
import unittest
from compare_framing import compare_frames


class CompareTest(unittest.TestCase):
    def compare(self, headers, captured):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        (root / 'framing').mkdir()
        first = dict(event='connection-direction', connection='conn1', direction='c2s',
                     source=['127.0.0.1', 55555], destination=['127.0.0.1', 1445])
        (root / 'framing/frames.jsonl').write_text('\n'.join(json.dumps(x) for x in [first, *captured]))
        (root / 'application-frames.jsonl').write_text('\n'.join(json.dumps(dict(
            event='read-header', actual=4, local='127.0.0.1:1445', remote='127.0.0.1:55555',
            connection=1, time='2026-01-01', previous_declared_length=71, **x)) for x in headers))
        return compare_frames(root)

    def test_valid_and_invalid_headers_match_from_connection_start(self):
        result = self.compare([dict(offset=0, header='00000047'), dict(offset=75, header='00000000')],
                              [dict(event='frame', connection='conn1', direction='c2s', offset=0, header='00000047'),
                               dict(event='invalid-frame', connection='conn1', direction='c2s', offset=75, header='00000000')])
        self.assertEqual(result['counts']['matching_headers'], 2)
        self.assertEqual(result['counts']['matching_invalid_headers'], 1)
        self.assertEqual(result['first_discrepancies'], [])

    def test_header_mismatch_and_missing_boundary_are_distinct(self):
        result = self.compare([dict(offset=0, header='00000047'), dict(offset=75, header='00000000')],
                              [dict(event='frame', connection='conn1', direction='c2s', offset=0, header='00000048')])
        self.assertEqual(result['counts']['different-header-at-same-offset'], 1)
        self.assertEqual(result['counts']['no-captured-boundary-at-application-offset'], 1)
        self.assertNotIn('00000047', json.dumps(result))


if __name__ == '__main__':
    unittest.main()
