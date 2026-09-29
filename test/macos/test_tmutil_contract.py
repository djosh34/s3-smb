#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Captured-output regression for the real platform documentation gate."""
import os
from pathlib import Path
import unittest
from unittest.mock import Mock, patch

for key in ('MAC_WORK', 'MAC_ARTIFACTS', 'MAC_RUNNER_HOME', 'MAC_BIN'):
    os.environ.setdefault(key, '/tmp/unused-mac-helper-default')
import acceptance
from native import verify_tmutil_verb

FIXTURES = Path(__file__).with_name('platform-fixtures')


class PastSetdestinationCheck(Exception):
    pass


class TmutilContract(unittest.TestCase):
    def setUp(self):
        self.manual = (FIXTURES / 'macos-15.7.9-man-contract-excerpt.txt').read_text()

    def test_recorded_help_reaches_next_check_in_real_platform_path(self):
        # Both inputs are from run36584525709, not an invented SMB-capable help.
        manual = (FIXTURES / 'macos-15.7.9-man-contract-excerpt.txt').read_text()
        help_text = (FIXTURES / 'macos-15.7.9-setdestination-help.txt').read_text()
        import hashlib
        self.assertEqual(hashlib.sha256(help_text.encode()).hexdigest(),
                         '7758f0367f1d027e38a4ea676be750175cd27618082cf7252629e4fe7338c25c')
        self.assertNotIn('smb', help_text)
        a = acceptance.Acceptance()
        def command(argv, **kwargs):
            if argv[:2] == ['/bin/sh', '-c']:
                return manual, 0
            if argv == ['/usr/bin/tmutil', 'help', 'setdestination']:
                return help_text, 0
            # Stop before any native command; crossing this boundary proves the
            # actual assertion under platform(), not a disconnected test helper.
            raise PastSetdestinationCheck()
        a.cmd = Mock()
        a.cmd.run.side_effect = command
        with patch.object(acceptance.sys, 'platform', 'darwin'), patch.object(acceptance.os, 'geteuid', return_value=0):
            with self.assertRaises(PastSetdestinationCheck):
                a.platform()

    def test_same_boundary_uses_documented_options_not_brief_help_spelling(self):
        a = acceptance.Acceptance()
        def command(argv, **kwargs):
            if argv[:2] == ['/bin/sh', '-c']:
                return self.manual, 0
            if argv[:2] == ['/usr/bin/tmutil', 'help']:
                if argv[2] == 'setdestination':
                    return (FIXTURES / 'macos-15.7.9-setdestination-help.txt').read_text(), 0
                # Intentionally abbreviated synthetic help: long-option or -X
                # omission is not native absence. Options come from the manual.
                return f'Usage: tmutil {argv[2]}\n', 0
            if argv == ['/usr/bin/tmutil', 'status']:
                return (FIXTURES / 'macos-15.7.9-status.txt').read_text(), 0
            raise PastSetdestinationCheck()
        a.cmd = Mock()
        a.cmd.run.side_effect = command
        with patch.object(acceptance.sys, 'platform', 'darwin'), patch.object(acceptance.os, 'geteuid', return_value=0):
            with self.assertRaises(PastSetdestinationCheck):
                a.platform()
        a.cmd.run.assert_any_call(['/usr/bin/tmutil', 'status'])
        self.assertNotIn((['/usr/bin/tmutil', 'help', 'status'],), [c.args for c in a.cmd.run.call_args_list])

    def test_missing_documented_smb_contract_still_fails(self):
        for manual in (self.manual.replace('SMB share', 'network share'),
                       self.manual.replace('protocol://user[:pass]@host/share', '')):
            with self.subTest(manual=manual):
                with self.assertRaisesRegex(RuntimeError, 'SMB destination URL'):
                    verify_tmutil_verb(manual, 'setdestination', [], 0)

    def test_missing_option_cannot_borrow_from_different_verb(self):
        manual = self.manual.replace('destinationinfo [-X]', 'destinationinfo')
        with self.assertRaisesRegex(RuntimeError, 'destinationinfo'):
            verify_tmutil_verb(manual, 'destinationinfo', ['-X'], 0)
        verify_tmutil_verb(self.manual, 'addexclusion', ['-p', '-v'], 0)
        with self.assertRaisesRegex(RuntimeError, 'startbackup'):
            verify_tmutil_verb(self.manual.replace('--block', '--other'), 'startbackup', ['--block'], 0)

    def test_missing_verb_and_failed_help_collection_still_fail(self):
        with self.assertRaisesRegex(RuntimeError, 'does not document'):
            verify_tmutil_verb(self.manual, 'unknown', [], 0)
        with self.assertRaisesRegex(RuntimeError, 'cannot collect'):
            verify_tmutil_verb(self.manual, 'setdestination', [], 77)

    def test_named_empty_mount_route_is_unchanged(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            a = acceptance.Acceptance()
            a.share = Path(directory) / 'smb'
            a.cmd = Mock()
            a.mount_share()
            a.cmd.run.assert_any_call(['/sbin/mount_smbfs', '-N', '//timemachine:@127.0.0.1/TimeMachine', a.share])

    def test_destination_uses_documented_empty_password_prompt(self):
        import plistlib
        a = acceptance.Acceptance()
        a.cmd = Mock()
        a.cmd.set_destination_empty_password.return_value = ''
        a.cmd.run.return_value = (plistlib.dumps({'Destinations': [{'ID': 'test-destination'}]}).decode(), 0)
        a.save = Mock()
        a.configure_destination()
        a.cmd.set_destination_empty_password.assert_called_once_with('smb://timemachine@127.0.0.1/TimeMachine')
        self.assertEqual(a.destination, 'test-destination')

    def test_destination_empty_dict_is_native_rejection_not_schema_error(self):
        import plistlib
        a = acceptance.Acceptance()
        a.cmd = Mock()
        a.cmd.set_destination_empty_password.return_value = ''
        a.cmd.run.return_value = (plistlib.dumps({}).decode(), 0)
        a.save = Mock()
        with self.assertRaisesRegex(RuntimeError, 'did not configure'):
            a.configure_destination()
        self.assertIsNone(a.destination)
        a.save.assert_called_once_with('destination.json', {})

    def test_destination_reported_failure_cannot_be_success_at_exit_zero(self):
        a = acceptance.Acceptance()
        a.cmd = Mock()
        a.cmd.set_destination_empty_password.return_value = '(null) (error 0)\\nThe backup destination could not be set.\\n'
        a.save = Mock()
        with self.assertRaisesRegex(RuntimeError, 'could not be set'):
            a.configure_destination()
        self.assertIsNone(a.destination)


if __name__ == '__main__':
    unittest.main()
