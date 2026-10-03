#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""One ordinary fresh Apple SMB Time Machine capability trial. No retries.

All stdout/stderr and native outputs go to authenticated private retention.
Only fixed-field observations are written to APPLE_PUBLIC. No offline raw parser.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess
import time
import traceback

from acceptance import Acceptance, WORK, EVIDENCE, TRANSFER, PROOF, SMB_PORT
from apple_tm_measure import BACKING_CAP, RAW_CAP, RESERVE, CIPHER_CAP, CAPTURE_BUFFER, budget_admitted, capacity, geometry, initial_result
from client_outcome import status_observations, collect

PUBLIC = Path(os.environ['APPLE_PUBLIC'])
RAW = WORK / 'capture.pcap'
META = EVIDENCE / 'capture-meta'
REPO = Path(__file__).resolve().parents[2]


def save(name, value):
    (PUBLIC / name).write_text(json.dumps(value, indent=2, allow_nan=False) + '\n')


class AppleTimeMachine(Acceptance):
    def __init__(self):
        super().__init__()
        self.password = secrets.token_urlsafe(32)
        self.result = initial_result()
        self.server = self.capture = self.sampler = None
        self.outer = WORK / 'backing.sparsebundle'
        self.outer_device = None
        self.backing_mount = Path('/private/var') / ('apple-tm-' + secrets.token_hex(8))
        self.directory = self.backing_mount / 'TimeMachine'
        self.start_wall = None
        self.end_wall = None
        self.share_created = False
        self.owns_clients = False

    def event(self, event, **fields):
        # Parent events can include native text/paths. Never mirror them publicly.
        with (EVIDENCE / 'events.jsonl').open('a') as f:
            f.write(json.dumps(dict(event=event, monotonic_ns=time.monotonic_ns(), **fields)) + '\n')

    def provenance(self):
        def command_value(argv, pattern):
            text, code = self.cmd.run(argv, timeout=30, diagnostic=True)
            value = text.strip()
            return value if code == 0 and re.fullmatch(pattern, value) else None
        def digest(path):
            h = hashlib.sha256()
            with Path(path).open('rb') as f:
                for chunk in iter(lambda: f.read(1 << 20), b''):
                    h.update(chunk)
            return h.hexdigest()
        sha = os.environ.get('APPLE_HARNESS_SHA', '')
        image = os.environ.get('APPLE_RUNNER_IMAGE_VERSION', '')
        save('provenance.json', dict(
            os_version=command_value(['/usr/bin/sw_vers', '-productVersion'], r'[0-9.]+'),
            os_build=command_value(['/usr/bin/sw_vers', '-buildVersion'], r'[0-9A-Za-z.]+'),
            architecture=command_value(['/usr/bin/uname', '-m'], r'x86_64|arm64'),
            harness_sha=sha if re.fullmatch(r'[0-9a-f]{40}', sha) else None,
            runner_image_version=image if re.fullmatch(r'[0-9.]+', image) else None,
            capture_binary_sha256=digest(WORK / 'passive-capture'),
            smbd_binary_sha256=digest('/usr/sbin/smbd'),
            direct_smb_port=SMB_PORT, requested_capture_buffer_bytes=CAPTURE_BUFFER))

    def capability(self):
        self.result['capability_step'] = 'physical-preflight'
        self.result['physical_before'] = capacity(WORK)
        self.result['physical_budget_admitted'] = budget_admitted(shutil.disk_usage(WORK).free)
        if not self.result['physical_budget_admitted']:
            raise RuntimeError('physical budget not admitted')
        self.result['capability_step'] = 'installed-smbd-help'
        help_text, code = self.cmd.run(['/usr/sbin/smbd', '-help'], diagnostic=True)
        self.result['smbd_ports_documented'] = bool(re.search(r'(?<!\w)-ports\b', help_text))
        self.result['smbd_help_exit'] = code
        if not self.result['smbd_ports_documented']:
            raise RuntimeError('installed smbd does not document ports; no topology fallback')
        self.result['capability_step'] = 'installed-provenance'
        for command in (['/usr/bin/man', 'smbd'], ['/usr/bin/man', 'hdiutil'],
                        ['/usr/bin/hdiutil', 'create', '-help'], ['/usr/bin/sw_vers'],
                        ['/usr/bin/uname', '-srm'], ['/sbin/route', '-n', 'get', '127.0.0.1'],
                        ['/sbin/pfctl', '-s', 'info'], ['/sbin/pfctl', '-s', 'nat'],
                        ['/usr/sbin/sysctl', 'kern.osversion', 'kern.uuid', 'hw.ncpu', 'hw.memsize'],
                        ['/usr/bin/shasum', '-a', '256', '/usr/sbin/smbd']):
            self.cmd.run(command, timeout=30, diagnostic=True)
        with socket.socket() as s:
            s.bind(('127.0.0.1', SMB_PORT))
        self.backing_mount.mkdir(mode=0o711)
        # Only outer server storage is created. backupd owns inner image creation.
        self.result['capability_step'] = 'outer-image-create'
        self.create_backing()
        self.result['capability_step'] = 'outer-image-attach'
        text, _ = self.cmd.run(['/usr/bin/hdiutil', 'attach', '-owners', 'on', '-nobrowse',
                                '-mountpoint', self.backing_mount, '-plist', self.outer], timeout=120)
        import plistlib
        entities = plistlib.loads(text.encode())['system-entities']
        devices = [e['dev-entry'] for e in entities if 'dev-entry' in e]
        mounted = [e['mount-point'] for e in entities if 'mount-point' in e]
        if mounted != [str(self.backing_mount)] or not devices:
            raise RuntimeError('unexpected owned backing attachment')
        self.outer_device = devices[0]
        self.result['local_backing_before'] = capacity(self.backing_mount)
        self.budget_check()
        self.cmd.run(['/usr/bin/tmutil', 'addexclusion', '-v', self.backing_mount])
        self.result['capability_step'] = 'owned-server-account'
        self.cmd.run(['/usr/sbin/sysadminctl', '-addUser', 'timemachine', '-password', self.password])
        # Standard SMB credential provisioning for this disposable server account.
        # No client, signing, kernel, firewall, or system authentication policy edit.
        self.cmd.run(['/usr/bin/pwpolicy', '-u', 'timemachine', '-sethashtypes', 'SMB-NT', 'on'])
        self.cmd.run(['/usr/bin/dscl', '.', '-passwd', '/Users/timemachine', self.password])
        self.cmd.run(['/usr/bin/tmutil', 'addexclusion', '-p', '/Users/timemachine'])
        self.directory.mkdir(mode=0o700)
        self.cmd.run(['/usr/sbin/chown', 'timemachine', self.directory])
        self.result['capability_step'] = 'owned-share'
        self.cmd.run(['/usr/sbin/sharing', '-a', self.directory, '-n', 'TimeMachine',
                      '-S', 'TimeMachine', '-s', '001', '-g', '000'])
        self.share_created = True
        self.cmd.run(['/usr/bin/dscl', '.', '-create', '/SharePoints/TimeMachine', 'timeMachineBackup', '1'])
        self.result['capability_step'] = 'direct-smbd-listener'
        self.server = self.service(['/usr/sbin/smbd', '-ports', str(SMB_PORT)], 'apple-smbd')
        deadline = time.monotonic() + 15
        while True:
            if self.server.poll() is not None:
                raise RuntimeError('owned direct-port smbd exited; no launchd/PF/relay fallback')
            try:
                with socket.create_connection(('127.0.0.1', SMB_PORT), timeout=1):
                    break
            except OSError:
                if time.monotonic() >= deadline:
                    raise RuntimeError('direct Apple SMB listener unavailable')
                time.sleep(.2)
        self.result['capability_step'] = 'complete'
        self.result['capability_stage'] = 'pass'

    def create_backing(self):
        initial_free = shutil.disk_usage(WORK).free
        with (EVIDENCE / 'backing-create.log').open('xb') as log:
            process = subprocess.Popen(['/usr/bin/hdiutil', 'create', '-size', '1p', '-type',
                                        'SPARSEBUNDLE', '-fs', 'HFS+J', '-volname',
                                        'AppleTMBacking', str(self.outer)], stdout=log,
                                       stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
            deadline = time.monotonic() + 300
            try:
                while process.poll() is None:
                    free = shutil.disk_usage(WORK).free
                    if initial_free - free > BACKING_CAP or free < RESERVE:
                        self.result['resource_guard_hit'] = True
                        raise RuntimeError('backing metadata allocation exceeded budget')
                    if time.monotonic() >= deadline:
                        raise RuntimeError('backing creation deadline')
                    time.sleep(.5)
                if process.returncode:
                    raise RuntimeError('backing creation failed')
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)

    def budget_check(self):
        free = shutil.disk_usage(WORK).free
        # Sparse outer allocation, never a traversal of backed file contents.
        allocated = sum(p.stat().st_blocks * 512 for p in (self.outer / 'bands').iterdir())
        raw_bytes = RAW.stat().st_size if RAW.exists() else 0
        # Leave enough room to retain an incompressible capture plus private logs.
        logs_bytes = sum(p.stat().st_size for p in EVIDENCE.rglob('*') if p.is_file())
        if allocated > BACKING_CAP or logs_bytes > 1 << 30 or free < RESERVE + CIPHER_CAP:
            self.result['resource_guard_hit'] = True
            self.result['workload_stop_reason'] = 'harness-storage-guard'
            raise RuntimeError('physical storage safety guard')
        return dict(backing_allocated_bytes=allocated, physical_free_bytes=free, raw_bytes=raw_bytes)

    def start_capture(self):
        META.mkdir(mode=0o700)
        self.capture = self.service([WORK / 'passive-capture', 'capture', 'lo0', 'smb',
                                    str(CAPTURE_BUFFER), RAW, META, str(RAW_CAP), '1200',
                                    str(RESERVE + CIPHER_CAP)], 'capture')
        deadline = time.monotonic() + 15
        while not (META / 'ready.json').exists():
            if self.capture.poll() is not None or time.monotonic() >= deadline:
                raise RuntimeError('capture not ready')
            time.sleep(.1)
        # Engine-generated metadata is reviewed separately; never raw/native text.
        shutil.copyfile(META / 'ready.json', PUBLIC / 'capture-ready.json')

    def observe(self, label):
        sample = dict(label=label, monotonic_ns=time.monotonic_ns(), wall_ns=time.time_ns())
        text, code = self.cmd.run(['/usr/bin/tmutil', 'status'], timeout=5, diagnostic=True)
        sample.update(status_exit=code, status=status_observations(text),
                      geometry=geometry(self.directory), **self.budget_check())
        with (PUBLIC / 'workload-samples.jsonl').open('a') as f:
            f.write(json.dumps(sample, allow_nan=False) + '\n')
        return sample

    def mount_share(self):
        self.owns_clients = True
        return super().mount_share()

    def ordinary_backup(self):
        self.mount_share()
        self.result['smb_capacity_before'] = capacity(self.share)
        if list(self.share.iterdir()):
            raise RuntimeError('new destination not empty')
        self.configure_destination()
        self.create_tree()
        self.check_exclusions()
        self.observe('before')
        from resource_sampler import NumericResourceSampler
        self.sampler = NumericResourceSampler(PUBLIC / 'resources.jsonl', WORK)
        self.sampler.set_pids(server=self.server.pid, capture=self.capture.pid)
        self.sampler.start()
        self.start_wall = time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(time.time() - 2))
        self.start_backup('ordinary')
        process, log = self.backup
        self.sampler.set_pids(backup_client=process.pid)
        self.result.update(start_monotonic_ns=time.monotonic_ns(), command_timeout=False,
                           workload_stop_reason='in-progress')
        deadline = time.monotonic() + 900
        next_sample = next_budget = 0
        while process.poll() is None:
            if self.server.poll() is not None or self.capture.poll() is not None:
                self.result['workload_stop_reason'] = ('server-process-exit' if self.server.poll() is not None
                                                       else 'harness-capture-exit')
                raise RuntimeError('owned observation/server exited during workload')
            if time.monotonic() >= deadline:
                self.result['command_timeout'] = True
                self.result['workload_stop_reason'] = 'harness-deadline'
                raise RuntimeError('ordinary backup exceeded fifteen minute bound')
            if time.monotonic() >= next_sample:
                self.observe('progress')
                next_sample = time.monotonic() + 60
            if time.monotonic() >= next_budget:
                self.budget_check()
                next_budget = time.monotonic() + 5
            time.sleep(.5)
        self.end_wall = time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(time.time() + 2))
        self.result.update(workload_stop_reason='normal-command-completion',
                           command_exit=process.returncode, end_monotonic_ns=time.monotonic_ns(),
                           elapsed_ms=round((time.monotonic() - self.backup_started) * 1000))
        log.close()
        self.backup = None
        self.sampler.set_pids(backup_client=None)
        after = self.observe('after-command')
        self.result['inner_geometry_after'] = after['geometry']
        if process.returncode or after['status']['running'] != 0:
            raise RuntimeError('backup command did not complete cleanly')
        self.detach_clients()
        self.stop_capture()
        self.stop_sampler()
        # Selection is separate from command success. No writable attach fallback.
        self.mount_share()
        selected = self.remote_backup('ordinary')
        self.result['selection_check'] = 'new-completed'
        self.tree_in_backup(selected, PROOF.relative_to('/'))
        self.result['created_tree'] = 'present'
        self.result['run_stage'] = 'pass'

    def attach(self, bundle, *flags):
        if '-readonly' not in flags:
            raise RuntimeError('ordinary control refuses writable recovery attachment')
        return super().attach(bundle, *flags)

    def stop_capture(self):
        if self.capture is None:
            return
        if self.capture.poll() is None:
            self.capture.send_signal(signal.SIGINT)
        try:
            code = self.capture.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.capture.kill()
            code = self.capture.wait(timeout=5)
        self.result['capture_exit'] = code
        if self.sampler:
            self.sampler.set_pids(capture=None)
        for source, target in [('summary.json', 'capture-summary.json'), ('samples.jsonl', 'capture-samples.jsonl')]:
            if (META / source).exists():
                shutil.copyfile(META / source, PUBLIC / target)
        if (META / 'summary.json').exists():
            summary = json.loads((META / 'summary.json').read_text())
            self.result['capture_health_valid'] = summary.get('capture_health_valid') is True and code == 0
        # Capture health never proves continuous stream coverage or correct SMB.
        self.result['measurement_stage'] = 'unknown'

    def stop_sampler(self):
        if self.sampler is None:
            return
        health = self.sampler.stop()  # Includes join, final flush/close errors.
        coverage = {role: 0 for role in ('server', 'capture', 'backup_client', 'sampler')}
        with (PUBLIC / 'resources.jsonl').open() as f:
            for line in f:
                row = json.loads(line)
                if row.get('kind') == 'sample':
                    for role in coverage:
                        coverage[role] += row['processes'][role]['status'] == 'ok'
        health['role_samples'] = coverage
        health['darwin_ps_observed'] = coverage['sampler'] > 0
        health['sampler_health_valid'] = (health['samples'] > 0 and all(coverage.values())
                                          and not health['observation_errors'] and not health['write_errors'])
        save('sampler-stop.json', health)
        self.result['sampler_health_valid'] = health['sampler_health_valid']

    def cleanup(self):
        failed = False
        def attempt(action):
            nonlocal failed
            try:
                action()
            except BaseException:
                traceback.print_exc()
                failed = True
        if self.backup:
            attempt(lambda: self.stop_backup_client())
        if self.owns_clients:
            attempt(self.detach_clients)
        attempt(self.stop_capture)
        if self.sampler:
            self.sampler.set_pids(backup_client=None)
            attempt(self.stop_sampler)
        if self.start_wall:
            end = self.end_wall or time.strftime('%Y-%m-%d %H:%M:%S', time.localtime())
            def logs():
                _, code = self.cmd.run(['/usr/bin/log', 'show', '--style', 'json', '--start', self.start_wall,
                                       '--end', end, '--info', '--debug', '--predicate',
                                       'process == "backupd" OR process == "backupd-helper"'],
                                      timeout=60, diagnostic=True, capture=False)
                private_log = EVIDENCE / f'{self.cmd.seq:04d}-log.log'
                self.result.update(client_log_collection_exit=code, client_log_timeout=code == 124,
                                   client_log_scope='attempt-window', client_observations=collect('unified', private_log))
            attempt(logs)
        for process, log in reversed(self.services):
            def stop(p=process, f=log):
                if p.poll() is None:
                    p.terminate()
                    try:
                        p.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        p.kill()
                        p.wait(timeout=5)
                f.close()
            attempt(stop)
        if self.share_created:
            attempt(lambda: self.cmd.run(['/usr/sbin/sharing', '-r', 'TimeMachine']))
        if self.outer_device:
            attempt(lambda: self.cmd.run(['/usr/bin/hdiutil', 'detach', self.outer_device], timeout=60))
        self.result['cleanup_stage'] = 'fail' if failed else 'pass'
        # Preserve every owned image, original and private log. No deletion here.

    def execute_trial(self):
        try:
            self.provenance()
            self.platform()
            if self.status():
                raise RuntimeError('unowned backup already active')
            (WORK / 'objects').mkdir()
            self.capability()
            self.start_capture()
            self.ordinary_backup()
        except BaseException:
            # Freeze the workload boundary before stopbackup/detach/observer cleanup.
            if self.start_wall and self.end_wall is None:
                self.end_wall = time.strftime('%Y-%m-%d %H:%M:%S', time.localtime())
                self.result['end_monotonic_ns'] = time.monotonic_ns()
                self.result['harness_aborted'] = self.backup is not None and self.backup[0].poll() is None
                if self.result['workload_stop_reason'] == 'in-progress':
                    self.result['workload_stop_reason'] = 'harness-error'
            traceback.print_exc()
            if self.result['capability_stage'] != 'pass':
                self.result['capability_stage'] = 'fail'
            else:
                self.result['run_stage'] = 'fail'
        finally:
            self.cleanup()
            save('trial.json', self.result)
        return 0 if self.result['run_stage'] == 'pass' and self.result['cleanup_stage'] == 'pass' else 1


if __name__ == '__main__':
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError('trial deadline')))
    signal.alarm(1500)
    raise SystemExit(AppleTimeMachine().execute_trial())
