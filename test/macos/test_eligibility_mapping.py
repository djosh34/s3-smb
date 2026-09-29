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
import sys
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

    def test_cr_lf_paths_use_single_queries_without_losing_source_contents(self):
        # Portable fake CLI, not the unknown filename from run36599466224.
        for separator in ('\r', '\n'):
            with self.subTest(separator=repr(separator)), tempfile.TemporaryDirectory() as temp:
                base = Path(temp)
                source = base / 'source'
                source.mkdir()
                ordinary = [source / f'ordinary{i:03d}' for i in range(100)]
                for path in ordinary:
                    path.write_bytes(b'abc')
                sdk = source / ('sdk' + separator)
                sdk.mkdir()
                (sdk / 'content').write_bytes(b'12345')
                excluded = source / ('z-excluded' + separator)
                excluded.mkdir()
                (excluded / 'not-eligible').write_bytes(b'native exclusion only')
                queries = []
                run = subprocess.run
                fake_cli = '''import sys
for path in sys.argv[2:]:
    decision = 'Excluded' if path == sys.argv[1] else 'Included'
    sys.stdout.buffer.write(f'[{decision}]  {path}\\n'.encode())
'''

                def tmutil(argv, **kwargs):
                    queries.append(argv)
                    return run([sys.executable, '-c', fake_cli, str(excluded), *argv[2:]], **kwargs)

                with patch('native.subprocess.run', side_effect=tmutil):
                    counts = eligible_source(source, base / 'inventory', base / 'raw')
                rows = {row['path']: row for row in map(json.loads, (base / 'inventory').read_text().splitlines())}
                self.assertEqual(set(rows), {str(p) for p in [source, *ordinary, sdk, sdk / 'content', excluded]})
                self.assertTrue(rows[str(sdk / 'content')]['included'])
                self.assertFalse(rows[str(excluded)]['included'])
                self.assertEqual(counts['files'], 101)
                self.assertEqual(counts['logical_bytes'], 305)
                self.assertEqual(counts['excluded'], 1)
                self.assertIn(100, [len(argv) - 2 for argv in queries])
                for argv in queries:
                    if any(separator in p for p in argv[2:]):
                        self.assertEqual(len(argv), 3)
                logged = [json.loads(line.removeprefix('# argv: '))
                          for line in (base / 'raw').read_text().splitlines() if line.startswith('# argv: ')]
                self.assertEqual(logged, queries)

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
