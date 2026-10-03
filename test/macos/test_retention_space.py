# SPDX-License-Identifier: AGPL-3.0-only
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
from retention_space import check


class RetentionSpaceTest(unittest.TestCase):
    def test_requires_reserve_without_assuming_compression(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / 'logs').mkdir()
            (root / 'public').mkdir()
            (root / 'raw').write_bytes(b'x' * 1000)
            (root / 'logs/log').write_bytes(b'y' * 1000)
            with patch('retention_space.shutil.disk_usage', return_value=Mock(free=1024)):
                self.assertFalse(check(root / 'raw', root / 'logs', root / 'public', 1024))
            result = json.loads((root / 'public/retention-space.json').read_text())
            self.assertGreater(result['ciphertext_upper_bound_bytes'], 2000)
            self.assertFalse(result['favorable_compression_assumed'])
            with patch('retention_space.shutil.disk_usage', return_value=Mock(free=10 * 2**20)):
                self.assertTrue(check(root / 'raw', root / 'logs', root / 'public', 1024))

    def test_logs_only_and_symlink_rejection(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / 'logs').mkdir()
            (root / 'public').mkdir()
            (root / 'logs/log').write_text('synthetic')
            with patch('retention_space.shutil.disk_usage', return_value=Mock(free=10 * 2**20)):
                self.assertTrue(check(root / 'absent', root / 'logs', root / 'public', 1024))
                (root / 'logs/link').symlink_to(root / 'logs/log')
                with self.assertRaises(RuntimeError):
                    check(root / 'absent', root / 'logs', root / 'public', 1024)


if __name__ == '__main__':
    unittest.main()
