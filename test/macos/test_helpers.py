#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Portable harness regressions, not Apple/MinIO acceptance evidence."""
import json
import os
from pathlib import Path
import shutil
import stat
import sys
import tempfile
import unittest
from unittest.mock import patch

from manifest import manifest, compare
from native import Commands, Daemon, capacity_requirement, eligible_source


class Helpers(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.tree = self.base / 'tree'
        self.tree.mkdir()
        (self.tree / 'file').write_bytes(b'ordinary contents\0not proof-file filtering')
        (self.tree / 'subdirectory').mkdir()
        (self.tree / 'subdirectory/another').write_text('all entries included')
        os.link(self.tree / 'file', self.tree / 'hardlink')
        os.symlink('file', self.tree / 'symlink')

    def snapshot(self, name='manifest.jsonl'):
        path = self.base / name
        manifest(self.tree, path)
        return path

    def change_and_reject(self, mutation):
        before = self.snapshot('before')
        mutation()
        after = self.snapshot('after')
        with self.assertRaisesRegex(RuntimeError, 'differences'):
            compare(before, after, self.base / 'differences')
        self.assertTrue((self.base / 'differences').stat().st_size)

    def test_full_content_and_link_topology(self):
        rows = [json.loads(x) for x in self.snapshot().read_text().splitlines()]
        self.assertEqual(len(rows), 6)
        by_path = {r['path']: r for r in rows}
        self.assertEqual(by_path['file']['sha256'], by_path['hardlink']['sha256'])
        self.assertEqual(by_path['hardlink']['hardlink_to'], 'file')
        self.assertEqual(by_path['symlink']['target'], 'file')
        self.assertIn('subdirectory/another', by_path)

    def test_reject_content_mutation(self):
        self.change_and_reject(lambda: (self.tree / 'subdirectory/another').write_text('changed'))

    def test_reject_missing_ordinary_entry(self):
        self.change_and_reject(lambda: (self.tree / 'subdirectory/another').unlink())

    def test_reject_added_entry(self):
        self.change_and_reject(lambda: (self.tree / 'extra').write_text('unexpected'))

    def test_reject_mode_mutation(self):
        self.change_and_reject(lambda: os.chmod(self.tree / 'file', 0o400))

    def test_reject_xattr_mutation(self):
        self.change_and_reject(lambda: os.setxattr(self.tree / 'file', 'user.proof', b'changed metadata'))

    def test_reject_split_hardlink(self):
        def split():
            src = self.tree / 'file'
            target = self.tree / 'hardlink'
            target.unlink()
            shutil.copy2(src, target)
        self.change_and_reject(split)

    def test_observational_fields_preserved_not_compared(self):
        before = self.snapshot('before')
        rows = [json.loads(x) for x in before.read_text().splitlines()]
        for row in rows:
            for field in ('inode', 'device', 'atime_ns', 'ctime_ns', 'nlink'):
                row[field] += 1
        after = self.base / 'after'
        after.write_text(''.join(json.dumps(row) + '\n' for row in rows))
        compare(before, after, self.base / 'diff')
        self.assertEqual((self.base / 'diff').stat().st_size, 0)

    def test_no_silent_read_failure(self):
        with patch('manifest.os.listxattr', side_effect=PermissionError('denied')):
            with self.assertRaises(PermissionError):
                self.snapshot()

    def test_eligibility_native_decisions_and_unique_hardlinks(self):
        excluded = self.tree / 'subdirectory'
        def native(argv, **kwargs):
            import subprocess
            output = ''.join(f'[{"Excluded" if Path(p) == excluded else "Included"}] {p}\n' for p in argv[2:])
            return subprocess.CompletedProcess(argv, 0, output.encode(), b'')
        with patch('native.subprocess.run', side_effect=native):
            counts = eligible_source(self.tree, self.base / 'source', self.base / 'raw')
        self.assertEqual(counts['files'], 2)
        self.assertEqual(counts['logical_bytes'], 2 * (self.tree / 'file').stat().st_size)
        self.assertEqual(counts['unique_hardlink_logical_bytes'], (self.tree / 'file').stat().st_size)
        self.assertEqual(counts['excluded'], 1)
        self.assertNotIn('subdirectory/another', (self.base / 'source').read_text())

    def test_eligibility_failure_is_not_zero_capacity(self):
        import subprocess
        with patch('native.subprocess.run', return_value=subprocess.CompletedProcess([], 1, b'', b'Full Disk Access denied')):
            with self.assertRaisesRegex(RuntimeError, 'isexcluded failed'):
                eligible_source(self.tree, self.base / 'source', self.base / 'raw')
        self.assertIn('denied', (self.base / 'raw').read_text())

    def test_command_failure_and_timeout_retained(self):
        commands = Commands(self.base)
        with self.assertRaisesRegex(RuntimeError, r'failed \(7\)'):
            commands.run([sys.executable, '-c', 'print("diagnostic"); raise SystemExit(7)'])
        with self.assertRaisesRegex(RuntimeError, r'failed \(124\)'):
            commands.run([sys.executable, '-c', 'import time; print("before timeout",flush=True); time.sleep(10)'], timeout=.2)
        rows = [json.loads(x) for x in (self.base / 'commands.jsonl').read_text().splitlines()]
        self.assertEqual([row['exit'] for row in rows], [7, 124])
        self.assertIn('before timeout', (self.base / rows[1]['output']).read_text())

    def test_capacity_estimate_is_explicit_arithmetic(self):
        self.assertEqual(capacity_requirement(12_000_000_000, 1000, 512_000_000), 33_024_000_000)
        self.assertGreater(capacity_requirement(1, 2_000_000, 0), 8_000_000_000)

    def test_pty_consent_and_continuous_drain(self):
        executable = self.base / 'fake-application'
        executable.write_text(f'''#!{sys.executable}
import os, signal, sys, time
signal.signal(signal.SIGTERM, lambda *args: sys.exit(0))
with open('/dev/tty','w') as tty:
    tty.write('Initialize a genuinely empty S3 dataset?\\nContinue? [yes/no]: ')
    tty.flush()
with open('/dev/tty') as tty:
    assert tty.readline() == 'yes\\n'
print('{{"msg":"SMB serving"}}', flush=True)
time.sleep(.3)
os.write(1,b'x'*1000000)
while True: time.sleep(.1)
''')
        executable.chmod(0o700)
        daemon = Daemon(executable, self.base / 'unused', self.base / 'pty.log', 'initialize')
        try:
            daemon.ready()
            import time
            time.sleep(.5)
            daemon.pump()
        finally:
            daemon.stop()
        self.assertGreater((self.base / 'pty.log').stat().st_size, 1_000_000)


if __name__ == '__main__':
    unittest.main()
