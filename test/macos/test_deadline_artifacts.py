#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Real local process-group/identity checks; no Apple/platform workaround."""
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import unittest

from artifacts import verify
from deadline import run_until
from native import Daemon


class DeadlineArtifacts(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)

    def test_shared_deadline_kills_stalled_build_and_child(self):
        marker = self.base / 'child-survived'
        child = f'import signal,time,pathlib; signal.signal(signal.SIGTERM,signal.SIG_IGN); time.sleep(1); pathlib.Path({str(marker)!r}).write_text("bad")'
        script = self.base / 'stalled-build.py'
        script.write_text(f'import subprocess,sys,time; subprocess.Popen([sys.executable,"-c",{child!r}]); time.sleep(30)')
        evidence = self.base / 'deadline.json'
        start = time.time()
        code = run_until([sys.executable, str(script)], start + .25, evidence, grace=.1)
        self.assertEqual(code, 124)
        self.assertLess(time.time() - start, 2)
        self.assertTrue(json.loads(evidence.read_text())['timed_out'])
        time.sleep(1.1)
        self.assertFalse(marker.exists(), 'compiler child outlived the shared deadline')

    def test_build_failure_is_not_changed_to_success(self):
        evidence = self.base / 'deadline.json'
        self.assertEqual(run_until([sys.executable, '-c', 'raise SystemExit(7)'], time.time() + 5, evidence), 7)
        self.assertFalse(json.loads(evidence.read_text())['timed_out'])

    def test_daemon_shutdown_timeout_kills_reaps_and_fails(self):
        script = self.base / 'ignore-term'
        script.write_text(f'''#!{sys.executable}
import signal,time
signal.signal(signal.SIGTERM,signal.SIG_IGN)
print('{{"msg":"SMB serving"}}',flush=True)
while True: time.sleep(.1)
''')
        script.chmod(0o700)
        daemon = Daemon(script, self.base / 'unused', self.base / 'pty.log', 'restart')
        daemon.ready()
        with self.assertRaisesRegex(RuntimeError, 'forced=True'):
            daemon.stop(timeout=.1)
        self.assertTrue(daemon.reaped)
        self.assertTrue(os.WIFSIGNALED(daemon.exit_status))
        with self.assertRaises(ChildProcessError):
            os.waitpid(daemon.pid, os.WNOHANG)
        self.assertTrue(daemon.log.closed)

    def test_real_root_private_artifact_handoff_to_uploader(self):
        # Required for this explicit integration test, not an optional skip.
        self.assertNotEqual(os.geteuid(), 0, 'run this test as the ordinary uploader, with sudo -n')
        evidence = self.base / 'evidence'
        evidence.mkdir(mode=0o700)
        producer = 'import os,sys; os.umask(0o077); open(sys.argv[1],"x").write("complete root evidence\\n")'
        root_log = evidence / 's3-events.jsonl'
        subprocess.run(['sudo', '-n', sys.executable, '-c', producer, str(root_log)], check=True)
        self.assertEqual(root_log.stat().st_uid, 0)
        with self.assertRaises(PermissionError):
            root_log.read_bytes()
        helper = Path(__file__).with_name('artifacts.py')
        subprocess.run(['sudo', '-n', sys.executable, str(helper), 'handoff', str(evidence),
                        str(os.geteuid()), str(os.getegid())], check=True)
        verify(evidence)  # Actual ordinary identity, same operation as workflow.
        self.assertEqual(stat.S_IMODE(root_log.stat().st_mode), 0o600)
        self.assertEqual(root_log.stat().st_uid, os.geteuid())
        root_log.write_text('truncated')
        with self.assertRaisesRegex(RuntimeError, 'content changed'):
            verify(evidence)


if __name__ == '__main__':
    unittest.main()
