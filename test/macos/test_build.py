#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Run the Mac build script with fake platform and compiler commands."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[2]
REVISION = '1234567890abcdef1234567890abcdef12345678'
MINIO_COMMIT = 'b' * 40
MINIO_RELEASE = 'RELEASE.2099-01-02T03-04-05Z'
STUB = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

command = Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ['COMMAND_LOG'], 'a') as log:
    log.write(json.dumps({'command': command, 'args': args, 'cwd': os.getcwd()}) + '\n')
if command == 'uname':
    print('Darwin')
elif command == 'git' and args == ['rev-parse', 'HEAD']:
    print(os.environ['REVISION'])
elif command == 'go':
    if args == ['env', 'GOVERSION']:
        print('go1.26.3')
    elif args and args[0] == 'build':
        output = Path(args[args.index('-o') + 1])
        if output.name == 's3-smb' and os.environ.get('FAIL_BUILD'):
            print('compiler failed', file=sys.stderr)
            sys.exit(1)
        output.write_text('#!/bin/bash\nexit 0\n')
        output.chmod(0o700)
    elif args[:2] == ['version', '-m']:
        if os.environ.get('FAIL_METADATA'):
            sys.exit(1)
        print('fake build metadata for ' + args[2])
'''


class Build(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.base = Path(temp.name)
        self.repo = self.base / 'repo'
        (self.repo / 'test').mkdir(parents=True)
        (self.repo / 'test/Dockerfile').write_text(
            f'ARG MINIO_RELEASE={MINIO_RELEASE}\nARG MINIO_COMMIT={MINIO_COMMIT}\n'
        )
        self.commands = self.base / 'commands'
        self.commands.mkdir()
        for command in ('uname', 'git', 'go', 'sw_vers', 'xcodebuild', 'xcrun', 'diskutil'):
            path = self.commands / command
            path.write_text(STUB)
            path.chmod(0o700)
        self.work = self.base / 'work'
        self.artifacts = self.base / 'evidence'
        self.work.mkdir()
        self.artifacts.mkdir()
        self.log = self.base / 'commands.jsonl'
        self.env = {
            **os.environ,
            'PATH': str(self.commands) + os.pathsep + os.environ['PATH'],
            'MAC_WORK': str(self.work),
            'MAC_ARTIFACTS': str(self.artifacts),
            'MAC_BIN': str(self.work / 'bin'),
            'COMMAND_LOG': str(self.log),
            'REVISION': REVISION,
        }

    def run_build(self, server, **env):
        return subprocess.run(
            ['/bin/bash', str(REPO / 'test/macos/build.sh')],
            cwd=self.repo, env={**self.env, 'MAC_SERVER': server, **env},
            capture_output=True, text=True, check=False,
        )

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_builds_checkout_and_records_evidence_for_each_server(self):
        # Use a fresh work directory for each build, as run.sh does.
        for server, tags in (('default', ''), ('smbnext', 'smbnext')):
            with self.subTest(server=server):
                self.env['MAC_BIN'] = str(self.work / server)
                result = self.run_build(server)
                self.assertEqual(result.returncode, 0, result.stderr)
                builds = [c for c in self.calls() if c['command'] == 'go' and c['args'][0] == 'build']
                application = builds[-3]
                self.assertEqual(application['cwd'], str(self.repo))
                self.assertEqual(application['args'], [
                    'build', '-p', '3', '-tags', tags, '-o', self.env['MAC_BIN'] + '/s3-smb', '.',
                ])
                self.assertNotIn('-tags', builds[-2]['args'])  # MinIO
                self.assertNotIn('-tags', builds[-1]['args'])  # Fixture
                for name in ('application-revision', 'harness-revision'):
                    self.assertEqual((self.artifacts / name).read_text(), REVISION + '\n')
                self.assertEqual((self.artifacts / 'build-tags').read_text(), tags + '\n')
                self.assertEqual((self.artifacts / 'native-build.txt').read_text(),
                                 'fake build metadata for ' + self.env['MAC_BIN'] + '/s3-smb\n')
                self.assertEqual((self.artifacts / 'minio-release').read_text(), MINIO_RELEASE + '\n')
                self.assertEqual((self.artifacts / 'minio-revision').read_text(), MINIO_COMMIT + '\n')
                fetches = [c['args'] for c in self.calls() if c['command'] == 'git' and 'fetch' in c['args']]
                self.assertEqual(fetches[-1][-1], MINIO_COMMIT)
                self.assertTrue((self.artifacts / 'application-build.log').is_file())
                self.assertEqual(sorted(p.name for p in self.work.iterdir()),
                                 ['default'] if server == 'default' else ['default', 'smbnext'])
        self.assertFalse(any(c['command'] == 'go' and c['args'][0] == 'install' for c in self.calls()))
        self.assertFalse((self.artifacts / 'public-install.log').exists())
        self.assertFalse((self.artifacts / 'application-version').exists())

    def test_rejects_unknown_server_before_building(self):
        result = self.run_build('other')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('MAC_SERVER must be default or smbnext', result.stderr)
        self.assertFalse(any(c['command'] == 'go' for c in self.calls()))

    def test_build_failure_stops_before_minio(self):
        result = self.run_build('default', FAIL_BUILD='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('compiler failed', (self.artifacts / 'application-build.log').read_text())
        self.assertFalse((self.artifacts / 'native-build.txt').exists())
        self.assertFalse(any(c['command'] == 'git' and 'fetch' in c['args'] for c in self.calls()))

    def test_metadata_failure_stops_before_minio(self):
        result = self.run_build('smbnext', FAIL_METADATA='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c['command'] == 'git' and 'fetch' in c['args'] for c in self.calls()))

    def test_rejects_invalid_minio_pin(self):
        (self.repo / 'test/Dockerfile').write_text('ARG MINIO_RELEASE=bad\nARG MINIO_COMMIT=bad\n')
        result = self.run_build('default')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c['command'] == 'git' and 'fetch' in c['args'] for c in self.calls()))

    def test_run_requires_server_selection(self):
        env = {**self.env, 'MAC_PHASE': 'discover', 'MAC_TRANSFER': str(self.base / 'transfer')}
        env.pop('MAC_SERVER', None)
        result = subprocess.run(
            ['/bin/bash', str(REPO / 'test/macos/run.sh')],
            env=env, capture_output=True, text=True, check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('MAC_SERVER', result.stderr)
        self.assertFalse(self.log.exists())


if __name__ == '__main__':
    unittest.main()
