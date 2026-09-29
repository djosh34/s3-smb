#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Review regressions; mocks are process-lifecycle evidence, not Mac proof."""
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch, call

for key in ('MAC_WORK', 'MAC_ARTIFACTS', 'MAC_RUNNER_HOME', 'MAC_BIN'):
    os.environ.setdefault(key, '/tmp/unused-mac-helper-default')
import acceptance


class Lifecycle(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        p = patch.object(acceptance, 'EVIDENCE', self.base)
        p.start()
        self.addCleanup(p.stop)
        self.a = acceptance.Acceptance()
        self.a.cmd = Mock()
        self.a.cmd.run.return_value = ('', 0)
        self.a.event = Mock()

    def test_daemon_failure_is_terminal_but_services_still_reaped(self):
        self.a.daemon = Mock(reaped=False, pid=42, exit_status=None)
        self.a.daemon.stop.side_effect = RuntimeError('shutdown failed')
        process, log = Mock(), Mock()
        process.poll.return_value = 1
        process.wait.return_value = 1
        process.args, process.pid = ['fixture'], 43
        self.a.services = [(process, log)]
        with self.assertRaisesRegex(RuntimeError, 'cleanup'):
            self.a.finish()
        process.wait.assert_called()
        log.close.assert_called_once()
        self.assertFalse(any(c.args[0] == 'acceptance-passed' for c in self.a.event.call_args_list))

    def test_service_exit_one_cannot_pass(self):
        process, log = Mock(), Mock()
        process.poll.return_value = 1
        process.wait.return_value = 1
        process.args, process.pid = ['fixture'], 43
        self.a.services = [(process, log)]
        with self.assertRaisesRegex(RuntimeError, 'cleanup'):
            self.a.finish()
        process.wait.assert_called()
        log.close.assert_called_once()

    def test_stopbackup_failure_reaps_owned_client_and_closes_log(self):
        process, log = Mock(), Mock()
        process.poll.return_value = None
        process.wait.return_value = -15
        process.pid = 44
        self.a.backup = (process, log)
        def run(argv, **kwargs):
            if argv == ['/usr/bin/tmutil', 'stopbackup']:
                raise RuntimeError('native stopbackup timeout')
            return ('', 0)
        self.a.cmd.run.side_effect = run
        with self.assertRaisesRegex(RuntimeError, 'cleanup'):
            self.a.finish()
        process.wait.assert_called()
        log.close.assert_called_once()
        self.assertIsNone(self.a.backup)

    def test_success_is_emitted_only_after_cleanup(self):
        order = []
        self.a.run = Mock(return_value={'baseline': 'b', 'resumed': 'r'})
        self.a.finish = Mock(side_effect=lambda: order.append('cleanup'))
        self.a.event.side_effect = lambda name, **kw: order.append(name)
        self.a.execute()
        self.assertEqual(order, ['cleanup', 'acceptance-passed'])
        self.a.finish.side_effect = RuntimeError('fixture exit 1')
        order.clear()
        with self.assertRaisesRegex(RuntimeError, 'acceptance failed'):
            self.a.execute()
        self.assertEqual(order, ['acceptance-failed'])

    def test_resumed_ordinary_loss_or_change_fails_before_restore(self):
        from manifest import manifest
        import shutil
        selected = self.base / 'resumed-id'
        (selected / 'Data').mkdir(parents=True)
        ordinary = selected / 'Data/ordinary-build-file'
        ordinary.write_text('independently completed data')
        (selected / 'Data/proof').write_text('unchanged supplemental proof')
        manifest(selected, self.base / 'resumed-before-wipe-manifest.jsonl')
        self.a.remote_backup = Mock(return_value=(selected, {}))
        work = self.base / 'work'
        work.mkdir()
        with patch.object(acceptance, 'WORK', work):
            for mutation in ('changed', 'lost'):
                with self.subTest(mutation=mutation):
                    if mutation == 'changed':
                        ordinary.write_text('wrong data after recovery')
                    else:
                        ordinary.unlink()
                    with self.assertRaisesRegex(RuntimeError, 'differences'):
                        self.a.restore_full('resumed', 'resumed-id', compare_to='resumed-before-wipe')
                    self.a.remote_backup.assert_called_with('resumed', 'resumed-id')
                    self.a.cmd.run.assert_not_called()
                    shutil.rmtree(work / 'restore')
                    for path in self.base.glob('resumed-*'):
                        if path.is_file() and path.name != 'resumed-before-wipe-manifest.jsonl':
                            path.unlink()

    def test_resumed_expectation_is_captured_before_wipe(self):
        names = ('platform', 'start_services', 'start_daemon', 'mount_share',
                 'configure_destination', 'source_changes', 'start_backup',
                 'metadata_point', 'cold_recover', 'restore_full',
                 'crash_during_later_backup', 'inventory')
        sequence = Mock()
        for name in names:
            setattr(self.a, name, getattr(sequence, name))
        self.a.share = self.base
        self.a.complete_backup = Mock(return_value='completion-time')
        self.a.baseline_before_wipe = Mock(return_value='baseline-id')
        self.a.completed_before_wipe = sequence.completed_before_wipe
        self.a.completed_before_wipe.side_effect = ['baseline-id', 'resumed-id']
        self.a.restore_full.return_value = 'resumed-id'
        self.a.run()
        calls = sequence.mock_calls
        capture = calls.index(call.completed_before_wipe('resumed'))
        wipe = calls.index(call.cold_recover('resumed'))
        self.assertLess(capture, wipe)
        self.assertIn(call.restore_full('resumed', 'resumed-id', compare_to='resumed-before-wipe'), calls)
        self.assertFalse(any(c.args[0] == 'acceptance-passed' for c in self.a.event.call_args_list))


if __name__ == '__main__':
    unittest.main()
