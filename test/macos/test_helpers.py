#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Portable created-tree/command regressions, not native acceptance evidence."""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from manifest import manifest, compare
from native import Commands, Daemon, progress


class Helpers(unittest.TestCase):
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

    def test_command_failure_and_timeout_retained(self):
        commands = Commands(self.base)
        with self.assertRaisesRegex(RuntimeError, r'failed \(7\)'):
            commands.run([sys.executable, '-c', 'print("diagnostic"); raise SystemExit(7)'])
        with self.assertRaisesRegex(RuntimeError, r'failed \(124\)'):
            commands.run([sys.executable, '-c', 'import time; print("before timeout",flush=True); time.sleep(10)'], timeout=.2)
        rows = [json.loads(x) for x in (self.base / 'commands.jsonl').read_text().splitlines()]
        self.assertEqual([row['exit'] for row in rows], [7, 124])
        self.assertIn('before timeout', (self.base / rows[1]['output']).read_text())

    def test_live_command_summary_does_not_print_arguments_or_output(self):
        import contextlib
        import io
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            Commands(self.base).run([sys.executable, '-c', 'print("synthetic-sensitive-output")'])
        self.assertIn('native-command-start', output.getvalue())
        self.assertIn('native-command-exit', output.getvalue())
        self.assertIn('code=0', output.getvalue())
        self.assertNotIn('synthetic-sensitive-output', output.getvalue())
        snapshot = self.base / 'progress.json'
        record = json.loads(snapshot.read_text())
        self.assertEqual(set(record), {'time', 'event', 'free_bytes', 'low_free_space', 'command', 'exit'})
        self.assertEqual(record['event'], 'native-command-exit')
        self.assertEqual(record['exit'], 0)
        self.assertGreaterEqual(record['free_bytes'], 0)
        self.assertNotIn('synthetic-sensitive-output', snapshot.read_text())
        self.assertEqual(snapshot.stat().st_mode & 0o777, 0o644)
        self.assertFalse((self.base / 'progress.json.tmp').exists())

    def test_progress_records_low_free_space_without_a_gate(self):
        from types import SimpleNamespace
        with patch('native.shutil.disk_usage', return_value=SimpleNamespace(free=1_000_000_000)):
            progress(self.base, 'time-machine-progress', scenario='password-control')
        record = json.loads((self.base / 'progress.json').read_text())
        self.assertEqual(record['free_bytes'], 1_000_000_000)
        self.assertTrue(record['low_free_space'])
        self.assertEqual(record['scenario'], 'password-control')

    def invoke_tmutil_prompt(self, body, timeout=2):
        executable = self.base / 'fake-tmutil.py'
        executable.write_text('import getpass, sys, time\n' + body)
        original_execv = os.execv
        def execute(command, argv):
            original_execv(sys.executable, [sys.executable, str(executable), *argv[1:]])
        with patch('native.os.execv', side_effect=execute):
            return Commands(self.base).set_destination_empty_password('smb://timemachine@127.0.0.1/TimeMachine', timeout=timeout)

    def test_real_pty_password_prompt_receives_only_empty_password(self):
        output = self.invoke_tmutil_prompt('''assert sys.argv[1:] == ['setdestination', '-p', 'smb://timemachine@127.0.0.1/TimeMachine']
assert getpass.getpass('Enter password: ') == ''
print('configured fixture only')
''')
        self.assertIn('configured fixture only', output)
        record = json.loads((self.base / 'commands.jsonl').read_text())
        self.assertEqual(record['exit'], 0)
        self.assertTrue(record['empty_password_answered'])
        with self.assertRaises(ProcessLookupError):
            os.kill(record['pid'], 0)

    def test_prompt_nonzero_exit_is_retained_and_fails(self):
        with self.assertRaisesRegex(RuntimeError, 'failed \\(22, answered=True\\)'):
            self.invoke_tmutil_prompt("assert getpass.getpass('Password: ') == ''\nprint('No password specified. (error -50)')\nsys.exit(22)\n")
        self.assertIn('No password specified.', (self.base / '0001-tmutil-password.log').read_text())

    def test_prompt_success_without_actual_prompt_is_not_pass(self):
        with self.assertRaisesRegex(RuntimeError, 'answered=False'):
            self.invoke_tmutil_prompt("print('no prompt')\n")

    def test_second_prompt_is_not_answered_and_process_is_reaped(self):
        with self.assertRaisesRegex(RuntimeError, 'failed \\(124, answered=True\\)'):
            self.invoke_tmutil_prompt("assert getpass.getpass('Password: ') == ''\ngetpass.getpass('Password: ')\n", timeout=.5)
        record = json.loads((self.base / 'commands.jsonl').read_text())
        self.assertEqual(record['exit'], 124)
        with self.assertRaises(ProcessLookupError):
            os.kill(record['pid'], 0)

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
