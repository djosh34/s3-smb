#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

from resource_sampler import INTERVAL_NS, NumericResourceSampler, parse_ps


class ResourceSamplerTests(unittest.TestCase):
    def sampler(self, root):
        sampler = NumericResourceSampler(Path(root) / 'numeric.jsonl', root)
        sampler.set_pids(sampler=None, server=123, capture=456)
        sampler._origin = time.monotonic_ns()
        return sampler

    def test_cpu_time_forms(self):
        for text, expected in [('0:01.25', 1.25), ('123:45.50', 7425.5),
                               ('01:02:03', 3723), ('2-01:02:03.50', 176523.5)]:
            row = parse_ps(f'123 125.5 2048 {text}\n', {123})[123]
            self.assertEqual(row['cpu_total_seconds'], expected)
            self.assertEqual(row['rss_kib'], 2048)
            self.assertEqual(row['cpu_percent'], 125.5)

    def test_reject_unexpected_or_non_numeric_output(self):
        for text in ['123 nan 4 0:00', '123 inf 4 0:00', '123 -1 4 0:00',
                     '123 1 -4 0:00', '123 1 4 0:60', '123 1 4 01:61:00',
                     '999 1 4 0:00', '123 1 4 0:00 secret', 'private error text',
                     '123 1 4 0:00\n123 1 4 0:00', '123 1e4 4 0:00', ' ' * 4097]:
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_ps(text, {123})
        self.assertEqual(parse_ps('', {123}), {})

    def test_fixed_roles_and_pid_validation(self):
        sampler = NumericResourceSampler('unused', '.')
        for kwargs in [dict(other=12), dict(server=True), dict(server=0), dict(server=-1),
                       dict(server='12')]:
            with self.assertRaises(ValueError):
                sampler.set_pids(**kwargs)
        sampler.set_pids(server=12, object_store=None)
        self.assertEqual(sampler._pids['server'], 12)
        self.assertEqual(sampler._pids['sampler'], os.getpid())

    def test_numeric_sample_argv_missingness_and_privacy(self):
        with tempfile.TemporaryDirectory() as root:
            sampler = self.sampler(root)
            result = subprocess.CompletedProcess([], 0, b'123 1.5 2048 0:01.20\n')
            with patch('resource_sampler.subprocess.run', return_value=result) as run:
                row = sampler._observe(sampler._origin)
            args, kwargs = run.call_args
            self.assertEqual(args[0], ['/bin/ps', '-p', '123,456', '-o', 'pid=,pcpu=,rss=,time='])
            self.assertEqual(kwargs['timeout'], 1)
            self.assertEqual(kwargs['stderr'], subprocess.DEVNULL)
            self.assertEqual(kwargs['env']['LC_ALL'], 'C')
            self.assertEqual(row['processes']['server']['status'], 'ok')
            self.assertEqual(row['processes']['capture']['status'], 'missing')
            self.assertIsNone(row['processes']['capture']['rss_kib'])
            self.assertEqual(row['processes']['object_store']['status'], 'unconfigured')
            self.assertIsNone(row['processes']['server']['threads'])
            self.assertIsNone(row['processes']['server']['read_bytes'])
            self.assertGreaterEqual(row['end_monotonic_ns'], row['start_monotonic_ns'])
            self.assertNotIn(root, json.dumps(row))

    def test_failures_never_emit_raw_errors(self):
        with tempfile.TemporaryDirectory() as root:
            sampler = self.sampler(root)
            cases = [subprocess.TimeoutExpired('PRIVATE', 1), OSError('PRIVATE'),
                     subprocess.CompletedProcess([], 2, b'PRIVATE'),
                     subprocess.CompletedProcess([], 0, b'PRIVATE'),
                     subprocess.CompletedProcess([], 0, b'\xff'),
                     subprocess.CompletedProcess([], 1, b'123 1 4 0:01')]
            for case in cases:
                with patch('resource_sampler.subprocess.run', **(
                        {'side_effect': case} if isinstance(case, Exception) else {'return_value': case})), \
                     patch('resource_sampler.shutil.disk_usage', side_effect=OSError('PRIVATE')), \
                     patch('resource_sampler.os.getloadavg', return_value=(float('nan'), 1, 2)):
                    row = sampler._observe(sampler._origin)
                self.assertIn(row['ps_status'], ('timeout', 'unavailable'))
                self.assertIsNone(row['processes']['server']['cpu_percent'])
                self.assertIsNone(row['disk_free_bytes'])
                self.assertIsNone(row['load_1_5_15'])
                self.assertNotIn('PRIVATE', json.dumps(row, allow_nan=False))
            self.assertEqual(sampler.summary()['observation_errors'], 18)

    def test_exit_and_no_configured_pids(self):
        with tempfile.TemporaryDirectory() as root:
            sampler = self.sampler(root)
            with patch('resource_sampler.subprocess.run', return_value=subprocess.CompletedProcess([], 1, b'')):
                row = sampler._observe(sampler._origin)
            self.assertEqual(row['processes']['server']['status'], 'missing')
            sampler.set_pids(server=None, capture=None)
            with patch('resource_sampler.subprocess.run') as run:
                row = sampler._observe(sampler._origin)
            run.assert_not_called()
            self.assertTrue(all(p['status'] == 'unconfigured' for p in row['processes'].values()))

    def test_cadence_skips_missed_intervals(self):
        with tempfile.TemporaryDirectory() as root:
            sampler = self.sampler(root)
            sampler._origin = 0
            sampler._file = io.StringIO()
            with patch.object(sampler, '_observe', return_value={'kind': 'sample'}), \
                 patch('resource_sampler.time.monotonic_ns', return_value=12_000_000_000), \
                 patch.object(sampler._stop, 'wait', side_effect=lambda seconds: sampler._stop.set()) as wait:
                sampler._run()
            self.assertEqual(sampler.summary()['missed_intervals'], 2)
            self.assertEqual(sampler.summary()['samples'], 1)
            wait.assert_called_once_with(3.0)
            self.assertEqual(INTERVAL_NS, 5_000_000_000)

    def test_write_flush_and_close_failure_health(self):
        class Broken(io.StringIO):
            def write(self, text):
                raise OSError('PRIVATE')
        with tempfile.TemporaryDirectory() as root:
            sampler = self.sampler(root)
            sampler._file = Broken()
            sampler._emit({'kind': 'sample'})
            self.assertEqual(sampler.summary()['write_errors'], 1)
            sampler._file = io.StringIO()
            with patch.object(sampler._file, 'flush', side_effect=OSError('PRIVATE')):
                sampler._emit({'kind': 'sample'})
            self.assertEqual(sampler.summary()['write_errors'], 2)
            sampler._thread = threading.Thread(target=lambda: None)
            sampler._thread.start()
            with patch.object(sampler._file, 'close', side_effect=OSError('PRIVATE')):
                self.assertEqual(sampler.stop()['write_errors'], 3)

    def test_lifecycle_permissions_and_exclusive_output(self):
        with tempfile.TemporaryDirectory() as root:
            sampler = self.sampler(root)
            sampled = threading.Event()
            def observe(scheduled):
                sampled.set()
                return {'kind': 'sample'}
            with patch.object(sampler, '_observe', side_effect=observe):
                sampler.start()
                self.assertTrue(sampled.wait(1))
                health = sampler.stop()
            self.assertEqual(health['samples'], 1)
            self.assertEqual(sampler.stop(), health)
            self.assertEqual(os.stat(sampler.output).st_mode & 0o777, 0o600)
            rows = [json.loads(line) for line in Path(sampler.output).read_text().splitlines()]
            self.assertEqual([r['kind'] for r in rows], ['sample', 'end'])
            with self.assertRaises(RuntimeError):
                sampler.start()
            with self.assertRaises(FileExistsError):
                NumericResourceSampler(sampler.output, root).start()

    def test_internal_failure_and_join_failure_visible(self):
        with tempfile.TemporaryDirectory() as root:
            sampler = self.sampler(root)
            sampler._file = io.StringIO()
            with patch.object(sampler, '_observe', side_effect=RuntimeError('PRIVATE')):
                sampler._run()
            self.assertEqual(sampler.summary()['samples'], 0)
            self.assertEqual(sampler.summary()['observation_errors'], 1)
            self.assertNotIn('PRIVATE', sampler._file.getvalue())
            class Unjoined:
                def join(self, timeout):
                    pass
                def is_alive(self):
                    return True
            sampler._thread = Unjoined()
            with self.assertRaisesRegex(RuntimeError, 'resource sampler did not stop'):
                sampler.stop()

    def test_own_pid_numeric_ps(self):
        # The only real process observed by the tests is this test process.
        with tempfile.TemporaryDirectory() as root:
            sampler = NumericResourceSampler(Path(root) / 'unused', root)
            sampler._origin = time.monotonic_ns()
            row = sampler._observe(sampler._origin)
            self.assertEqual(row['ps_status'], 'ok')
            self.assertEqual(row['processes']['sampler']['status'], 'ok')
            self.assertGreater(row['processes']['sampler']['rss_kib'], 0)


if __name__ == '__main__':
    unittest.main()
