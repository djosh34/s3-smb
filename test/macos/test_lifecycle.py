#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Portable lifecycle regressions; not Mac backup/restore evidence."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch, call

for key in ('MAC_WORK', 'MAC_ARTIFACTS', 'MAC_RUNNER_HOME', 'MAC_BIN'):
    os.environ.setdefault(key, '/tmp/unused-mac-helper-default')
import acceptance
from artifacts import handoff, INVENTORY
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

    def test_success_only_after_cleanup_and_export(self):
        order = []
        self.a.run = Mock(return_value={'baseline': 'b'})
        self.a.finish = Mock(side_effect=lambda: order.append('cleanup'))
        self.a.export_store = Mock(side_effect=lambda result: order.append('export'))
        self.a.event.side_effect = lambda name, **kw: order.append(name)
        with patch.dict(os.environ, MAC_PHASE='backup'):
            self.a.execute()
            self.assertEqual(order, ['cleanup', 'export', 'acceptance-passed'])
            self.a.finish.side_effect = RuntimeError('fixture exit 1')
            order.clear()
            self.a.export_store.reset_mock()
            with self.assertRaisesRegex(RuntimeError, 'acceptance failed'):
                self.a.execute()
            self.assertEqual(order, ['acceptance-failed'])
            self.a.export_store.assert_not_called()

    def test_export_moves_only_stopped_objects_without_a_second_copy(self):
        (self.work / 'objects/.minio.sys/empty').mkdir(parents=True)
        payload = self.work / 'objects/.minio.sys/payload'
        payload.write_bytes(b'object-store')
        inode = payload.stat().st_ino
        self.a.local.mkdir()
        (self.a.local / 'database').write_bytes(b'must not transfer')
        self.a.daemon = Mock(reaped=True)
        self.a.export_store(dict(baseline='b'))
        moved = self.transfer / 'store/objects/.minio.sys/payload'
        self.assertEqual(moved.read_bytes(), b'object-store')
        self.assertEqual(moved.stat().st_ino, inode)
        self.assertTrue((moved.parent / 'empty').is_dir())
        self.assertFalse((self.work / 'objects').exists())
        self.assertEqual({p.name for p in (self.transfer / 'store').iterdir()}, {'objects', 'recovery.json', 'store-stopped.json'})
        self.assertTrue((self.a.local / 'database').exists())
        self.assertTrue(json.loads((self.transfer / 'store/store-stopped.json').read_text())['clean_shutdown'])

    def test_export_rejects_running_application(self):
        self.a.daemon = Mock(reaped=False)
        with self.assertRaisesRegex(RuntimeError, 'active storage'):
            self.a.export_store({})
        self.assertFalse((self.transfer / 'store/objects').exists())

    def test_export_never_falls_back_to_copying_across_filesystems(self):
        source = self.work / 'objects'
        source.mkdir()
        self.a.daemon = Mock(reaped=True)
        with patch.object(Path, 'rename', side_effect=OSError('cross-device rename')):
            with self.assertRaises(OSError):
                self.a.export_store({})
        self.assertTrue(source.exists())
        self.assertFalse((self.transfer / 'store/objects').exists())
        self.assertFalse((self.transfer / 'store/store-stopped.json').exists())

    def test_backup_waits_native_point_before_cleanup(self):
        sequence = Mock()
        for name in ('platform', 'start_services', 'start_daemon', 'mount_share', 'configure_destination',
                     'observe_task_usage', 'confirm_backup_checkpoint', 'start_backup', 'complete_backup', 'detach_clients', 'remote_backup', 'metadata_point'):
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

    def received_store(self, scenario='named-empty'):
        root = self.transfer / 'store'
        (root / 'objects/.minio.sys/empty/nested').mkdir(parents=True)
        (root / 'objects/.minio.sys/payload').write_bytes(b'surviving S3 object')
        (root / 'store-stopped.json').write_text('{"clean_shutdown": true}')
        (root / 'recovery.json').write_text(json.dumps(dict(baseline='baseline', scenario=scenario)))
        handoff(root, os.geteuid(), os.getegid())
        return root

    def test_recovery_rejects_local_state_and_unsafe_inventory_paths(self):
        root = self.received_store()
        inventory = json.loads((root / INVENTORY).read_text())
        for name in ('daemon/database', '../outside', '/tmp/outside', 'objects/../../outside', '.', 'objects//bad'):
            with self.subTest(path=name):
                bad = dict(inventory, entries=inventory['entries'] + [dict(path=name, directory=True)])
                (root / INVENTORY).write_text(json.dumps(bad))
                with self.assertRaisesRegex(RuntimeError, 'inventory path'):
                    self.a.recover_phase()
        self.assertFalse(self.a.local.exists())
        self.assertFalse((self.work / 'objects').exists())

    def test_recovery_rejects_links_missing_changed_and_extra_bytes(self):
        root = self.received_store()
        self.a.platform = Mock()
        payload = root / 'objects/.minio.sys/payload'
        for fault in ('link', 'missing', 'changed', 'extra'):
            with self.subTest(fault=fault):
                if fault == 'link':
                    payload.unlink()
                    payload.symlink_to(self.base / 'outside')
                elif fault == 'missing':
                    payload.unlink()
                elif fault == 'changed':
                    payload.write_bytes(b'changed')
                else:
                    payload.write_bytes(b'surviving S3 object')
                    (root / 'unexpected').write_bytes(b'not inventoried')
                with self.assertRaises(RuntimeError):
                    self.a.recover_phase()
                self.assertFalse((self.work / 'objects').exists())
                self.a.platform.assert_not_called()

    def test_fresh_recovery_restores_empty_dirs_and_moves_verified_hidden_objects(self):
        root = self.received_store()
        payload = root / 'objects/.minio.sys/payload'
        inode = payload.stat().st_ino
        # Model the official SDK dropping empty directories during transport.
        (root / 'objects/.minio.sys/empty/nested').rmdir()
        (root / 'objects/.minio.sys/empty').rmdir()
        recovery = json.loads((root / 'recovery.json').read_text())
        sequence = Mock()
        for name in ('platform', 'start_services', 'start_daemon', 'mount_share', 'restore_tree', 'detach_clients'):
            setattr(self.a, name, getattr(sequence, name))
        self.assertEqual(self.a.recover_phase(), recovery)
        self.assertEqual(sequence.mock_calls, [call.platform(), call.start_services(fresh=False),
                         call.start_daemon('recover'), call.mount_share(), call.restore_tree(recovery), call.detach_clients()])
        moved = self.work / 'objects/.minio.sys/payload'
        self.assertEqual(moved.read_bytes(), b'surviving S3 object')
        self.assertEqual(moved.stat().st_ino, inode)
        self.assertTrue((moved.parent / 'empty/nested').is_dir())
        self.assertFalse((root / 'objects').exists())
        self.assertFalse(self.a.local.exists())

    def test_recovery_cannot_mix_authentication_scenarios(self):
        self.received_store(scenario='password-control')
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

    def test_task_usage_is_two_bounded_task_only_observations(self):
        targets = [str(self.work / 'objects'), str(self.a.local), str(self.base)]
        self.a.cmd.run.return_value = ('\n'.join(f'{n}\t{p}' for n, p in enumerate(targets, 1)) + '\n999\t/Users\n', 0)
        self.a.observe_task_usage()
        self.assertEqual(self.a.task_bytes, dict(task_store_bytes=1024, task_daemon_bytes=2048, task_evidence_bytes=3072))
        self.a.cmd.run.assert_called_once_with(['/usr/bin/du', '-sk', *targets], timeout=30, diagnostic=True)
        self.a.cmd.run.return_value = ('unavailable', 124)
        self.a.observe_task_usage()  # Timeout is unknown, not a backup gate.
        self.assertEqual(self.a.task_bytes, {})
        self.a.observe_task_usage()
        self.assertEqual(self.a.cmd.run.call_count, 2)

    def test_safe_numeric_fields_reach_progress_snapshot(self):
        self.a.task_bytes = dict(task_store_bytes=1024, task_daemon_bytes=2048, task_evidence_bytes=3072)
        acceptance.Acceptance.event(self.a, 'time-machine-progress', tm_percent=.5, tm_bytes=10,
                                    tm_total_bytes=20, native_status='not-public')
        snapshot = json.loads((self.base / 'progress.json').read_text())
        for key, value in {**self.a.task_bytes, 'tm_percent': .5, 'tm_bytes': 10, 'tm_total_bytes': 20}.items():
            self.assertEqual(snapshot[key], value)
        self.assertNotIn('native_status', snapshot)

    def test_heartbeat_observes_native_numbers_and_task_usage_once_after_five_minutes(self):
        process, log = Mock(returncode=0), Mock()
        process.poll.side_effect = [None, None, 0]
        self.a.backup = (process, log)
        self.a.daemon = Mock()
        self.a.status = Mock(return_value=False)
        self.a.observe_task_usage = Mock()
        status = 'Percent = 0.25;\nbytes = 100;\ntotalBytes = 400;'
        self.a.cmd.run.return_value = (status, 0)
        with patch.object(acceptance.time, 'monotonic', side_effect=[0, 0, 301, 362]), patch.object(acceptance.time, 'sleep'):
            self.a.complete_backup('baseline')
        self.a.observe_task_usage.assert_called_once()
        self.a.event.assert_any_call('time-machine-progress', label='baseline', native_status=status,
                                     exit=0, tm_percent=.25, tm_bytes=100, tm_total_bytes=400)

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
