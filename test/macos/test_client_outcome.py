#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from client_outcome import CLOCK_WARNING, collect, status_observations, unified_observations


def record(message, process='/private/synthetic/backupd'):
    return dict(processImagePath=process, eventMessage=message,
                timestamp='PRIVATE_TIMESTAMP', secret='PRIVATE_SENTINEL')


class StatusTest(unittest.TestCase):
    def test_known_progress(self):
        output = status_observations('''Backup session status:
{
 Running = 1;
 BackupPhase = MountingDiskImage;
 Progress = {
 Percent = "0.25";
 bytes = 1073741824;
 totalBytes = 4294967296;
 };
}''')
        self.assertEqual(output, dict(parse='ok', running=1, phase='MountingDiskImage',
                                     percent=.25, bytes=1073741824, total_bytes=4294967296))

    def test_not_running_is_not_success(self):
        self.assertEqual(status_observations('{\n Running = 0;\n}')['running'], 0)
        self.assertNotIn('success', status_observations('{\n Running = 0;\n}'))

    def test_unrecognized_format(self):
        for text in ('PRIVATE_SENTINEL', 'Running = 2;', 'Running = 0;\nRunning = 1;'):
            self.assertEqual(status_observations(text)['parse'], 'invalid')

    def test_unknown_phase_and_invalid_numbers(self):
        text = 'Running = 1;\nBackupPhase = PRIVATE_SENTINEL;\nPercent = 1e999;\nbytes = -3;\ntotalBytes = ' + '9' * 3000 + ';'
        output = status_observations(text)
        self.assertEqual(output['phase'], 'unknown')
        self.assertTrue(all(output[key] is None for key in ('percent', 'bytes', 'total_bytes')))
        self.assertNotIn('PRIVATE_SENTINEL', json.dumps(output))
        self.assertEqual(status_observations('Running = 1;\nBackupPhase = FuturePhase;')['phase'], 'other')

    def test_duplicate_progress_is_unknown(self):
        self.assertIsNone(status_observations('Running = 1;\nbytes = 1;\nbytes = 2;')['bytes'])


class UnifiedTest(unittest.TestCase):
    def test_failure_record_not_double_counted(self):
        output = unified_observations(json.dumps([record('BACKUP_FAILED_DISCONNECTED_NETWORK (26) PRIVATE_SENTINEL')]))
        self.assertEqual(output['disconnected_network_records'], 1)
        self.assertEqual(output['other_backup_failure_records'], 0)
        self.assertNotIn('PRIVATE_', json.dumps(output))

    def test_success_and_failure_do_not_cancel(self):
        output = unified_observations(json.dumps([record('BACKUP_FAILED_DISCONNECTED_NETWORK (26)'),
                                                 record('Backup completed successfully')]))
        self.assertEqual(output['disconnected_network_records'], 1)
        self.assertEqual(output['backup_success_records'], 1)
        self.assertNotIn('backup_completed', output)

    def test_clock_warning_is_explicit(self):
        for newline in ('\n', '\r\n'):
            output = unified_observations(CLOCK_WARNING + newline + json.dumps([record('Backup completed successfully')]))
            self.assertEqual(output['parse'], 'ok')
            self.assertTrue(output['clock_warning'])
            self.assertEqual(output['backup_success_records'], 1)

    def test_invalid_json_is_not_zero_failures(self):
        for text in ('', 'unknown preamble\n[]', '[{}', '{}', '[null]', '["PRIVATE_SENTINEL"]'):
            output = unified_observations(text)
            self.assertEqual(output['parse'], 'invalid')
            self.assertIsNone(output['disconnected_network_records'])

    def test_empty_collection_is_not_proven_coverage(self):
        output = unified_observations('[]')
        self.assertEqual(output['records'], 0)
        self.assertNotIn('coverage_complete', output)
        self.assertNotIn('backup_completed', output)

    def test_allowlisted_processes_only(self):
        output = unified_observations(json.dumps([record('Backup completed successfully', '/private/other'),
                                                 record('BACKUP_FAILED_DISCONNECTED_NETWORK', None)]))
        self.assertEqual(output['backup_success_records'], 0)
        self.assertEqual(output['backup_process_records'], 0)

    def test_band_and_image_are_numeric_only(self):
        output = unified_observations(json.dumps([record('Using a band size of 8.59 GB (on a volume with size of 1.13 PB) PRIVATE_SENTINEL'),
                                                 record('Disk image resized to 16000000000000 bytes')]))
        self.assertEqual((output['band_value'], output['band_unit']), (8.59, 'GB'))
        self.assertEqual(output['image_bytes'], 16000000000000)
        self.assertNotIn('PRIVATE_', json.dumps(output))
        self.assertNotIn('format_complete', output)

    def test_other_failure_name_not_exported(self):
        output = unified_observations(json.dumps([record('BACKUP_FAILED_PRIVATE_SENTINEL')]))
        self.assertEqual(output['other_backup_failure_records'], 1)
        self.assertNotIn('PRIVATE_', json.dumps(output))

    def test_invalid_row_types_ignored(self):
        output = unified_observations(json.dumps([record(None), {}, record(['PRIVATE_SENTINEL'])]))
        self.assertEqual(output['backup_process_records'], 0)


class FileTest(unittest.TestCase):
    def test_private_input_errors_are_fixed(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'PRIVATE_SENTINEL'
            self.assertEqual(collect('unified', path), dict(parse='read-error'))
            path.write_bytes(b'\xff')
            self.assertEqual(collect('unified', path), dict(parse='encoding-error'))
            path.write_bytes(b'[]' + b' ' * 20)
            with patch('client_outcome.MAX_INPUT', 8):
                self.assertEqual(collect('unified', path), dict(parse='oversize'))
            self.assertEqual(path.read_bytes(), b'[]' + b' ' * 20)
            path.write_bytes(b'[]')
            self.assertEqual(collect('unified', path)['parse'], 'ok')


if __name__ == '__main__':
    unittest.main()
