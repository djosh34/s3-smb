#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Tests for the tree manifest and its compare."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from manifest import manifest, compare


class Manifest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.tree = self.base / 'tree'
        (self.tree / 'nested/deeper').mkdir(parents=True)
        (self.tree / 'empty').mkdir()
        (self.tree / 'nested/empty').mkdir()
        (self.tree / 'file').write_bytes(b'independent test data\0')
        (self.tree / 'nested/deeper/file').write_text('nested test data')
        (self.tree / 'zero').touch()

    def snapshot(self, name='manifest.jsonl'):
        path = self.base / name
        manifest(self.tree, path)
        return path

    def change_and_reject(self, mutation):
        before = self.snapshot('before')
        mutation()
        after = self.snapshot('after')
        with self.assertRaisesRegex(RuntimeError, 'created-tree differences'):
            compare(before, after, self.base / 'differences')
        return [json.loads(line) for line in (self.base / 'differences').read_text().splitlines()]

    def test_nested_files_and_empty_directories(self):
        rows = {row['path']: row for row in map(json.loads, self.snapshot().read_text().splitlines())}
        self.assertEqual(len(rows), 8)
        for directory in ('.', 'empty', 'nested', 'nested/empty', 'nested/deeper'):
            self.assertEqual(rows[directory]['type'], 'directory')
        self.assertEqual(rows['zero']['bytes'], 0)
        self.assertEqual(rows['nested/deeper/file']['type'], 'file')
        self.assertEqual(set(rows['file']), {'path', 'type', 'bytes', 'sha256'})

    def test_changed_content(self):
        rows = self.change_and_reject(lambda: (self.tree / 'nested/deeper/file').write_text('changed'))
        self.assertEqual([row['path'] for row in rows], ['nested/deeper/file'])

    def test_missing_file(self):
        self.change_and_reject(lambda: (self.tree / 'file').unlink())

    def test_extra_file(self):
        self.change_and_reject(lambda: (self.tree / 'extra').write_text('extra'))

    def test_missing_empty_directory(self):
        self.change_and_reject(lambda: (self.tree / 'nested/empty').rmdir())

    def test_wrong_entry_type(self):
        def mutate():
            (self.tree / 'empty').rmdir()
            (self.tree / 'empty').touch()
        self.change_and_reject(mutate)

    def test_symlink_is_not_followed(self):
        def mutate():
            (self.tree / 'empty').rmdir()
            os.symlink(self.base, self.tree / 'empty')
        rows = self.change_and_reject(mutate)
        self.assertEqual(len(rows), 1)
        self.assertTrue(rows[0]['actual']['type'].startswith('unexpected:'))

    def test_metadata_and_order_not_gates(self):
        before = self.snapshot('before')
        os.chmod(self.tree / 'file', 0o400)
        after = self.snapshot('after')
        after.write_text('\n'.join(reversed(after.read_text().splitlines())) + '\n')
        compare(before, after, self.base / 'diff')
        self.assertEqual((self.base / 'diff').stat().st_size, 0)

    def test_no_silent_read_failure(self):
        with patch('manifest.hashlib.sha256', side_effect=OSError('read failed')):
            with self.assertRaises(OSError):
                self.snapshot()

    def test_outside_tree_is_not_compared(self):
        before = self.snapshot('before')
        (self.base / 'ordinary-system-file').write_text('not test data')
        after = self.snapshot('after')
        compare(before, after, self.base / 'diff')


if __name__ == '__main__':
    unittest.main()
