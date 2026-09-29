#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Portable same-file examples, not a replay of Mac run36590782555's query.

That run recorded an alternate Xcode spelling, but not its query argv/identity.
Only tmutil is mocked here; path identity and inventory traversal are real.
"""
import json
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from native import eligible_source


class EligibilityMapping(unittest.TestCase):
    def test_same_file_response_preserves_queried_paths_and_native_decisions(self):
        for included in (True, False):
            with self.subTest(included=included), tempfile.TemporaryDirectory() as temp:
                base = Path(temp)
                source = base / 'source'
                source.mkdir()
                (source / 'ordinary').write_bytes(b'abc')
                app = source / 'Xcode.app'
                app.mkdir()
                (app / 'SDK').write_bytes(b'12345')
                alias = base / 'alternate'
                alias.symlink_to(app, target_is_directory=True)

                def tmutil(argv, **kwargs):
                    lines = []
                    for path in argv[2:]:
                        decision = 'Excluded' if path == str(app) and not included else 'Included'
                        reported = alias if path == str(app) else path
                        lines.append(f'[{decision}]  {reported}\n')
                    return subprocess.CompletedProcess(argv, 0, ''.join(lines).encode(), b'')

                with patch('native.subprocess.run', side_effect=tmutil):
                    counts = eligible_source(source, base / 'inventory', base / 'raw')
                rows = {row['path']: row for row in map(json.loads, (base / 'inventory').read_text().splitlines())}
                expected = {str(source), str(source / 'ordinary'), str(app)}
                if included:
                    expected.add(str(app / 'SDK'))
                    self.assertTrue(stat.S_ISDIR(rows[str(app)]['mode']))
                self.assertEqual(set(rows), expected)
                self.assertEqual(rows[str(app)]['included'], included)
                self.assertEqual(counts['files'], 2 if included else 1)
                self.assertEqual(counts['logical_bytes'], 8 if included else 3)
                self.assertEqual(counts['excluded'], 0 if included else 1)
                queries = [json.loads(line.removeprefix('# argv: '))
                           for line in (base / 'raw').read_text().splitlines() if line.startswith('# argv: ')]
                self.assertIn(['/usr/bin/tmutil', 'isexcluded', str(app), str(source / 'ordinary')], queries)

    def test_unrelated_response_cannot_become_source_coverage(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            source = base / 'source'
            source.mkdir()
            (source / 'file').write_bytes(b'abc')
            unrelated = base / 'unrelated'
            unrelated.mkdir()
            response = f'[Included]  {unrelated}\n'.encode()
            with patch('native.subprocess.run', return_value=subprocess.CompletedProcess([], 0, response, b'')):
                with self.assertRaisesRegex(RuntimeError, 'unrecognized native eligibility result'):
                    eligible_source(source, base / 'inventory', base / 'raw')
            self.assertEqual((base / 'inventory').read_text(), '')
            self.assertIn(response, (base / 'raw').read_bytes())


if __name__ == '__main__':
    unittest.main()
