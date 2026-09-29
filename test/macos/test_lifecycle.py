#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Portable lifecycle regressions; not Mac backup/restore evidence."""
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch, call

for key in ('MAC_WORK', 'MAC_ARTIFACTS', 'MAC_RUNNER_HOME', 'MAC_BIN'):
    os.environ.setdefault(key, '/tmp/unused-mac-helper-default')
import acceptance
from manifest import manifest


class Lifecycle(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.work = self.base / 'work'
        self.transfer = self.base / 'transfer'
        self.work.mkdir()
        for part in ('store', 'reference'):
            (self.transfer / part).mkdir(parents=True)
        for name, value in (('EVIDENCE', self.base), ('WORK', self.work), ('TRANSFER', self.transfer)):
            patcher = patch.object(acceptance, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = patch.dict(os.environ, MAC_PHASE='recover', MAC_SCENARIO='named-empty')
        patcher.start()
        self.addCleanup(patcher.stop)
        self.a = acceptance.Acceptance()
        self.a.cmd = Mock()
        self.a.cmd.run.return_value = ('', 0)
        self.a.event = Mock()

    def test_daemon_failure_is_terminal_but_services_still_reaped(self):
        self.a.daemon = Mock(reaped=False, pid=42, exit_status=None)
        self.a.daemon.stop.side_effect = RuntimeError('shutdown failed')
        self.a.detach_clients = Mock()
        process, log = Mock(), Mock()
        process.poll.return_value = 1
        process.wait.return_value = 1
        process.args, process.pid = ['fixture'], 43
        self.a.services = [(process, log)]
        with self.assertRaisesRegex(RuntimeError, 'cleanup'):
            self.a.finish()
        process.wait.assert_called()
        log.close.assert_called_once()

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

    def test_success_only_after_cleanup_and_archive(self):
        order = []
        self.a.run = Mock(return_value={'baseline': 'b'})
        self.a.finish = Mock(side_effect=lambda: order.append('cleanup'))
        self.a.archive_store = Mock(side_effect=lambda result: order.append('archive'))
        self.a.event.side_effect = lambda name, **kw: order.append(name)
        with patch.dict(os.environ, MAC_PHASE='backup'):
            self.a.execute()
            self.assertEqual(order, ['cleanup', 'archive', 'acceptance-passed'])
            self.a.finish.side_effect = RuntimeError('fixture exit 1')
            order.clear()
            self.a.archive_store.reset_mock()
            with self.assertRaisesRegex(RuntimeError, 'acceptance failed'):
                self.a.execute()
            self.assertEqual(order, ['acceptance-failed'])
            self.a.archive_store.assert_not_called()

    def test_archive_contains_only_stopped_objects(self):
        (self.work / 'objects/bucket').mkdir(parents=True)
        (self.work / 'objects/bucket/payload').write_bytes(b'object-store')
        self.a.local.mkdir()
        (self.a.local / 'database').write_bytes(b'must not transfer')
        self.a.daemon = Mock(reaped=True)
        self.a.archive_store(dict(baseline='b'))
        with tarfile.open(self.transfer / 'store/minio.tar.gz') as archive:
            self.assertEqual(archive.getnames(), ['objects', 'objects/bucket', 'objects/bucket/payload'])
        self.assertTrue(json.loads((self.transfer / 'store/store-stopped.json').read_text())['clean_shutdown'])

    def test_archive_rejects_running_application(self):
        self.a.daemon = Mock(reaped=False)
        with self.assertRaisesRegex(RuntimeError, 'active storage'):
            self.a.archive_store({})
        self.assertFalse((self.transfer / 'store/minio.tar.gz').exists())

    def test_backup_waits_native_point_before_cleanup(self):
        sequence = Mock()
        for name in ('platform', 'start_services', 'start_daemon', 'mount_share', 'configure_destination',
                     'confirm_backup_checkpoint', 'start_backup', 'complete_backup', 'detach_clients', 'remote_backup', 'metadata_point'):
            setattr(self.a, name, getattr(sequence, name))
        self.a.share.mkdir()
        self.a.create_tree = Mock(return_value=Path('/Users/runner/s3-smb-acceptance-proof'))
        self.a.complete_backup.return_value = 'completed'
        self.a.remote_backup.return_value = Path('/remote/baseline')
        import plistlib
        self.a.cmd.run.return_value = (plistlib.dumps(dict(VolumeName='Data')).decode(), 0)
        result = self.a.backup_phase()
        self.assertEqual(result['baseline'], 'baseline')
        calls = sequence.mock_calls
        self.assertLess(calls.index(call.confirm_backup_checkpoint()), calls.index(call.start_backup('baseline')))
        self.assertLess(calls.index(call.complete_backup('baseline')), calls.index(call.metadata_point('completed')))
        self.assertEqual(calls[-2:], [call.detach_clients(), call.metadata_point('completed')])
        self.assertEqual(self.a.start_services.call_args, call(fresh=True))

    def test_fresh_recovery_rejects_old_local_state(self):
        self.a.local.mkdir()
        with self.assertRaisesRegex(RuntimeError, 'fresh daemon state'):
            self.a.recover_phase()
        self.a.cmd.run.assert_not_called()

    def test_recovery_rejects_local_state_in_archive(self):
        (self.transfer / 'store/store-stopped.json').write_text('{"clean_shutdown": true}')
        (self.transfer / 'store/recovery.json').write_text('{"scenario": "named-empty"}')
        with tarfile.open(self.transfer / 'store/minio.tar.gz', 'w:gz') as archive:
            entry = tarfile.TarInfo('daemon/database')
            entry.size = 0
            archive.addfile(entry)
        self.a.platform = Mock()
        with self.assertRaisesRegex(RuntimeError, 'archive member'):
            self.a.recover_phase()
        self.assertFalse(self.a.local.exists())

    def test_fresh_recovery_extracts_only_objects_then_recovers(self):
        (self.transfer / 'store/store-stopped.json').write_text('{"clean_shutdown": true}')
        recovery = dict(baseline='baseline', scenario='named-empty')
        (self.transfer / 'store/recovery.json').write_text(json.dumps(recovery))
        payload = self.base / 'payload'
        payload.write_bytes(b'surviving S3 object')
        with tarfile.open(self.transfer / 'store/minio.tar.gz', 'w:gz') as archive:
            archive.add(payload, arcname='objects/bucket/payload')
        sequence = Mock()
        for name in ('platform', 'start_services', 'start_daemon', 'mount_share', 'restore_tree', 'detach_clients'):
            setattr(self.a, name, getattr(sequence, name))
        self.assertEqual(self.a.recover_phase(), recovery)
        self.assertEqual(sequence.mock_calls, [call.platform(), call.start_services(fresh=False),
                         call.start_daemon('recover'), call.mount_share(), call.restore_tree(recovery), call.detach_clients()])
        self.assertEqual((self.work / 'objects/bucket/payload').read_bytes(), payload.read_bytes())
        self.assertFalse(self.a.local.exists())

    def test_recovery_cannot_mix_authentication_scenarios(self):
        (self.transfer / 'store/store-stopped.json').write_text('{"clean_shutdown": true}')
        (self.transfer / 'store/recovery.json').write_text('{"scenario": "password-control"}')
        self.a.platform = Mock()
        with self.assertRaisesRegex(RuntimeError, 'scenario does not match'):
            self.a.recover_phase()
        self.a.platform.assert_not_called()
        self.a.cmd.run.assert_not_called()

    def test_password_control_config_is_explicit_and_events_labelled(self):
        with patch.dict(os.environ, MAC_SCENARIO='password-control'):
            instance = acceptance.Acceptance()
        with patch.object(acceptance, 'Daemon', return_value=Mock(pid=42)):
            instance.start_daemon('initialize')
        self.assertIn('password: "synthetic-tm-control"', (instance.local / 'config.yaml').read_text())
        self.assertIn('listen: 127.0.0.1:1445', (instance.local / 'config.yaml').read_text())
        event = json.loads((self.base / 'acceptance.jsonl').read_text())
        self.assertEqual(event['scenario'], 'password-control')

    def test_restore_calls_native_only_for_created_tree(self):
        selected = self.base / 'remote/baseline'
        proof = selected / 'Data/Users/runner/s3-smb-acceptance-proof'
        (proof / 'nested/empty').mkdir(parents=True)
        (proof / 'file').write_bytes(b'independent')
        (selected / 'Data/ordinary-system-file').write_text('not compared')
        manifest(proof, self.transfer / 'reference/tree.jsonl')
        self.a.remote_backup = Mock(return_value=selected)
        recovery = dict(baseline='baseline', source_relative='Users/runner/s3-smb-acceptance-proof', source_volume_name='Data')
        def native(argv, **kwargs):
            self.assertEqual(argv[:3], ['/usr/bin/tmutil', 'restore', '-v'])
            self.assertEqual(argv[3], proof)
            # Mock only the native command's effect; production has no copy path.
            import shutil
            shutil.copytree(argv[3], argv[4])
            return '', 0
        self.a.cmd.run.side_effect = native
        self.a.restore_tree(recovery)
        self.a.cmd.run.assert_called_once()
        self.a.remote_backup.assert_called_once_with('normal', 'baseline', inherit=True)
        self.assertEqual((self.base / 'tree-differences.jsonl').read_text(), '')

    def test_detach_task_mount_with_explicit_smb_port(self):
        import plistlib
        mountpoint = str(self.work / 'smb')
        mounted = f'//timemachine@127.0.0.1:1445/TimeMachine on {mountpoint} (smbfs, nodev)\n'
        self.a.cmd.run.side_effect = [(plistlib.dumps({'images': []}).decode(), 0),
                                     (mounted, 0), ('', 0), ('', 0)]
        self.a.detach_clients()
        self.a.cmd.run.assert_any_call(['/sbin/umount', mountpoint], timeout=120)

    def test_before_backup_requires_upload_of_exact_checkpoint(self):
        from native import progress
        self.a.event.side_effect = lambda name: progress(self.base, name, scenario='named-empty')
        self.a.daemon = Mock()
        acknowledgment = self.work / 'progress-uploaded.json'
        with patch('native.utc', return_value='checkpoint-time'):
            acknowledgment.write_text('{"time": "checkpoint-time"}')
            self.a.confirm_backup_checkpoint()
        self.a.daemon.pump.assert_not_called()
        for stale in (None, 'previous-checkpoint'):
            with self.subTest(acknowledgment=stale):
                if stale is None:
                    acknowledgment.unlink()
                else:
                    acknowledgment.write_text(json.dumps({'time': stale}))
                with patch('native.utc', return_value='checkpoint-time'), patch.object(acceptance.time, 'monotonic', side_effect=[0, 181]):
                    with self.assertRaisesRegex(RuntimeError, 'checkpoint was not uploaded within 180'):
                        self.a.confirm_backup_checkpoint()

    def test_backup_progress_is_bounded_diagnostic_not_success_gate(self):
        process, log = Mock(), Mock()
        process.poll.side_effect = [None, None, 0]
        process.returncode = 0
        self.a.backup = process, log
        self.a.daemon = Mock()
        self.a.status = Mock(return_value=False)
        self.a.cmd.run.return_value = ('temporary status failure', 1)
        with patch.object(acceptance.time, 'monotonic', side_effect=[0, 0, 0, 1]), patch.object(acceptance.time, 'sleep'):
            self.a.complete_backup('baseline')
        self.a.cmd.run.assert_called_once_with(['/usr/bin/tmutil', 'status'], diagnostic=True)
        self.a.event.assert_any_call('time-machine-progress', label='baseline', native_status='temporary status failure', exit=1)
        log.close.assert_called_once()
        self.assertIsNone(self.a.backup)

    def test_normal_run_does_not_include_crash(self):
        self.a.backup_phase = Mock(return_value='backup')
        self.a.recover_phase = Mock(return_value='recover')
        with patch.dict(os.environ, MAC_PHASE='backup'):
            self.assertEqual(self.a.run(), 'backup')
        with patch.dict(os.environ, MAC_PHASE='recover'):
            self.assertEqual(self.a.run(), 'recover')
        with patch.dict(os.environ, MAC_PHASE='crash'):
            with self.assertRaisesRegex(RuntimeError, 'crash'):
                self.a.run()


if __name__ == '__main__':
    unittest.main()
