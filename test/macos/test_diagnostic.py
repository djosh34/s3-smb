# SPDX-License-Identifier: AGPL-3.0-only
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

for key in ('MAC_WORK', 'MAC_ARTIFACTS', 'MAC_RUNNER_HOME', 'MAC_BIN'):
    os.environ.setdefault(key, '/nonexistent-test-only')
import diagnostic


class DiagnosticTest(unittest.TestCase):
    def setUp(self):
        save = patch.object(diagnostic.Acceptance, 'save',
            lambda obj, name, data: (diagnostic.EVIDENCE / name).write_text(json.dumps(data)))
        save.start()
        self.addCleanup(save.stop)

    def configured(self, root):
        obj = diagnostic.Diagnostic()
        for name in ('budget_preflight', 'platform', 'start_capture', 'start_resources', 'start_services', 'start_daemon',
                     'mount_share', 'configure_destination', 'check_exclusions',
                     'detach_clients', 'tree_in_backup', 'sample_geometry'):
            setattr(obj, name, Mock())
        obj.create_tree = Mock(return_value=Path('/synthetic-proof'))
        obj.resources = Mock()
        obj.services = [(Mock(pid=111), None)]
        obj.daemon = Mock(pid=222)
        process = Mock(pid=333)
        process.poll.return_value = 0
        obj.start_backup = lambda label: setattr(obj, 'backup', (process, None))
        obj.complete_backup = Mock()
        obj.remote_backup = Mock(return_value=Path('/synthetic-backup'))
        obj.cmd.run = Mock(return_value=('123 objects', 0))
        return obj, process

    def test_completed_selection_is_not_restore(self):
        with tempfile.TemporaryDirectory() as root, patch.object(diagnostic, 'EVIDENCE', Path(root)):
            obj, _ = self.configured(root)
            obj.run()
            outcome = json.loads((Path(root) / 'workload-outcome.json').read_text())
            self.assertTrue(outcome['run_stage_completed'])
            self.assertTrue(outcome['tree_directory_present'])
            self.assertFalse(outcome['restore_performed'])
            self.assertFalse(outcome['content_hash_verified'])

    def test_selection_failure_preserves_command_completion(self):
        with tempfile.TemporaryDirectory() as root, patch.object(diagnostic, 'EVIDENCE', Path(root)):
            obj, _ = self.configured(root)
            obj.remote_backup.side_effect = RuntimeError('selection failed')
            with self.assertRaises(RuntimeError):
                obj.run()
            outcome = json.loads((Path(root) / 'workload-outcome.json').read_text())
            self.assertTrue(outcome['tm_command_completed'])
            self.assertFalse(outcome['run_stage_completed'])
            self.assertFalse(outcome['completed_backup_selected'])
            self.assertEqual(outcome['stage'], 'backup_selection')

    def test_command_failure_not_recast_as_success(self):
        with tempfile.TemporaryDirectory() as root, patch.object(diagnostic, 'EVIDENCE', Path(root)):
            obj, process = self.configured(root)
            process.poll.return_value = 1
            obj.complete_backup.side_effect = RuntimeError('command failed')
            with self.assertRaises(RuntimeError):
                obj.run()
            outcome = json.loads((Path(root) / 'workload-outcome.json').read_text())
            self.assertFalse(outcome['tm_command_completed'])
            self.assertEqual(outcome['tm_command_exit'], 1)

    def test_finish_defers_parser_and_never_asserts_coverage(self):
        with tempfile.TemporaryDirectory() as root, patch.object(diagnostic, 'EVIDENCE', Path(root)), \
                patch.object(diagnostic.Acceptance, 'finish'):
            (Path(root) / 'capture').mkdir()
            (Path(root) / 'capture/summary.json').write_text('{}')
            obj = diagnostic.Diagnostic()
            obj.capture = Mock()
            obj.capture.poll.return_value = None
            obj.capture.wait.return_value = 0
            obj.capture_log = Mock()
            obj.capture_ready = {'effective_buffer_bytes': 524288}
            obj.finish()
            measurement = json.loads((Path(root) / 'measurement.json').read_text())
            self.assertEqual(measurement['offline_analysis'], 'not_run_pending_authenticated_retention')
            self.assertFalse(measurement['whole_stream_coverage_proven'])
            self.assertFalse(measurement['semantic_correctness_proven'])
            self.assertFalse((Path(root) / 'framing').exists())

    def test_low_space_is_preflight_not_workload_failure(self):
        with tempfile.TemporaryDirectory() as root, patch.object(diagnostic, 'EVIDENCE', Path(root)), \
                patch.object(diagnostic.shutil, 'disk_usage', return_value=Mock(free=100 * 2**30)):
            obj = diagnostic.Diagnostic()
            with self.assertRaises(RuntimeError):
                obj.budget_preflight()
            budget = json.loads((Path(root) / 'budget-preflight.json').read_text())
            self.assertFalse(budget['admitted'])
            self.assertFalse(obj.outcome['run_stage_completed'])
            self.assertFalse(obj.outcome['tm_command_completed'])

    def test_event_does_not_print_native_status(self):
        with tempfile.TemporaryDirectory() as root, patch.object(diagnostic, 'EVIDENCE', Path(root)), \
                patch('builtins.print') as output:
            obj = diagnostic.Diagnostic()
            obj.sample_geometry = Mock()
            obj.sample_store_usage = Mock()
            obj.event('time-machine-progress', native_status='PRIVATE-SENTINEL')
            output.assert_called_once_with('time-machine-progress', flush=True)


if __name__ == '__main__':
    unittest.main()
