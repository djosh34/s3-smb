#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Full native backup on Mac A; created-tree restore on fresh Mac B."""
import datetime
import json
import os
from pathlib import Path
import plistlib
import re
import signal
import subprocess
import sys
import tarfile
import time
import traceback
import urllib.request

from manifest import manifest, compare
from native import Commands, Daemon, utc, verify_tmutil_verb, progress, tm_status_numbers

WORK = Path(os.environ['MAC_WORK']).resolve()
EVIDENCE = Path(os.environ['MAC_ARTIFACTS']).resolve()
HOME = Path(os.environ['MAC_RUNNER_HOME']).resolve()
BIN = Path(os.environ['MAC_BIN']).resolve()
TRANSFER = Path(os.environ.get('MAC_TRANSFER', str(WORK / 'transfer'))).resolve()
# Apple SMBClient explicitly permits local servers on nonstandard SMB ports;
# its NetFS path rejects local 139/445 by default (unlike mount_smbfs).
SMB_PORT = 1445
SMB_SERVER = f'127.0.0.1:{SMB_PORT}'


class Acceptance:
    def __init__(self):
        self.scenario = os.environ.get('MAC_SCENARIO', 'named-empty')
        if self.scenario not in ('named-empty', 'password-control'):
            raise RuntimeError('MAC_SCENARIO must be named-empty or password-control')
        # Public synthetic loopback fixture only. The control is not evidence
        # that Apple's Time Machine accepts the separately tested empty route.
        self.password = 'synthetic-tm-control' if self.scenario == 'password-control' else ''
        self.cmd = Commands(EVIDENCE)
        self.daemon = None
        self.services = []
        self.backup = None
        self.attachments = []
        self.share = WORK / 'smb'
        self.local = WORK / 'daemon'
        self.fixture = BIN / 'fixture'
        self.serial = 0
        self.destination = None
        self.task_bytes = {}
        self.task_usage_count = 0

    def event(self, event, **fields):
        # Latest of at most two bounded task-only observations, not fresh scans
        # on every heartbeat. Missing observations remain unknown.
        fields = {**self.task_bytes, **fields}
        fields.setdefault('scenario', self.scenario)
        with (EVIDENCE / 'acceptance.jsonl').open('a') as f:
            f.write(json.dumps(dict(time=utc(), event=event, **fields), sort_keys=True) + '\n')
        progress(EVIDENCE, event, scenario=self.scenario, tm_percent=fields.get('tm_percent'),
                 tm_bytes=fields.get('tm_bytes'), tm_total_bytes=fields.get('tm_total_bytes'),
                 task_store_bytes=fields.get('task_store_bytes'), task_daemon_bytes=fields.get('task_daemon_bytes'),
                 task_evidence_bytes=fields.get('task_evidence_bytes'))
        print(event, fields, flush=True)

    def save(self, name, data):
        with (EVIDENCE / name).open('x') as f:
            json.dump(data, f, indent=2, sort_keys=True)

    def control(self):
        with urllib.request.urlopen('http://127.0.0.1:19002/state', timeout=15) as response:
            state = json.load(response)
        if not state['evidence_ok']:
            raise RuntimeError('fixture evidence writer failed')
        return state

    def platform(self):
        if sys.platform != 'darwin' or os.geteuid() != 0:
            raise RuntimeError('native Darwin administrative execution required')
        manual, _ = self.cmd.run(['/bin/sh', '-c', 'MANPAGER=cat MANWIDTH=160 man tmutil | col -b'])
        required = {
            'setdestination': ['-p'], 'destinationinfo': ['-X'],
            'startbackup': ['--block', '--destination'], 'stopbackup': [],
            'listbackups': ['-d', '-m'], 'latestbackup': ['-d', '-m'],
            'restore': ['-v'], 'isexcluded': [], 'addexclusion': ['-p'],
        }
        for verb, options in required.items():
            _, code = self.cmd.run(['/usr/bin/tmutil', 'help', verb], diagnostic=True)
            verify_tmutil_verb(manual, verb, options, code)
        self.status()
        self.cmd.run(['/sbin/mount'])
        _, code = self.cmd.run(['/bin/launchctl', 'print', 'system/com.apple.backupd'], diagnostic=True)
        if code:
            self.cmd.run(['/bin/launchctl', 'enable', 'system/com.apple.backupd'])
            self.cmd.run(['/bin/launchctl', 'bootstrap', 'system', '/System/Library/LaunchDaemons/com.apple.backupd.plist'])
        self.cmd.run(['/bin/launchctl', 'print', 'system/com.apple.backupd'])
        # CI control/credential paths are excluded narrowly by run.sh. Never
        # exclude ordinary checkout, SDK, build, or user data to shorten backup.
        paths = (WORK, EVIDENCE, TRANSFER)
        self.save('test-exclusions.json', [dict(path=str(p), reason='task infrastructure; prevent recursive backup') for p in paths])
        for path in paths:
            self.cmd.run(['/usr/bin/tmutil', 'addexclusion', '-p', path])
        self.cmd.run(['/bin/df', '-k'])
        self.event('platform-ready')

    def service(self, argv, name):
        log = open(EVIDENCE / (name + '.log'), 'xb', buffering=0)
        process = subprocess.Popen([str(x) for x in argv], stdout=log, stderr=subprocess.STDOUT)
        self.services.append((process, log))
        return process

    def start_services(self, fresh):
        import socket
        for port in (SMB_PORT, 19000, 19001, 19002, 19003):
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', port))
        # Public synthetic values only: this isolated loopback fixture has no
        # real account. These same documented inputs are reconstructed on B.
        os.environ['MINIO_ROOT_USER'] = 'mac-acceptance'
        os.environ['MINIO_ROOT_PASSWORD'] = 'synthetic-mac-acceptance-secret'
        self.service([BIN / 'minio', 'server', '--address', '127.0.0.1:19000',
                      '--console-address', '127.0.0.1:19003', WORK / 'objects'], 'minio')
        deadline = time.monotonic() + 60
        while True:
            if any(p.poll() is not None for p, _ in self.services):
                raise RuntimeError('MinIO exited during startup')
            try:
                with urllib.request.urlopen('http://127.0.0.1:19000/minio/health/ready', timeout=2) as response:
                    if response.status == 200:
                        break
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= deadline:
                    raise RuntimeError('MinIO readiness deadline exceeded')
                time.sleep(.2)
        if fresh:
            self.cmd.run([self.fixture, 'bucket-create', '--endpoint', 'http://127.0.0.1:19000', '--bucket', 'time-machine'])
            output, _ = self.cmd.run([self.fixture, 'bucket-list', '--endpoint', 'http://127.0.0.1:19000', '--bucket', 'time-machine'])
            data = json.loads(output)
            self.save('initial-empty-bucket.json', data)
            if data['object_count'] != 0:
                raise RuntimeError('initial bucket is not empty')
        self.service([self.fixture, 'serve', '--upstream', 'http://127.0.0.1:19000',
                      '--listen', '127.0.0.1:19001', '--control', '127.0.0.1:19002',
                      '--events', EVIDENCE / 's3-events.jsonl'], 'proxy')
        deadline = time.monotonic() + 30
        while True:
            try:
                self.control()
                break
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= deadline:
                    raise RuntimeError('fixture readiness deadline exceeded')
                time.sleep(.2)

    def start_daemon(self, phase):
        self.serial += 1
        self.local.mkdir(mode=0o700, exist_ok=False)
        config = self.local / 'config.yaml'
        config.write_text(f'''smb:
  listen: {SMB_SERVER}
  share: TimeMachine
  username: timemachine
  password: "{self.password}"
storage:
  state_dir: "{self.local / 'state'}"
  cache_dir: "{self.local / 'cache'}"
  cache_size: 0
s3:
  endpoint: http://127.0.0.1:19001
  bucket: time-machine
  region: us-east-1
  path_style: true
  access_key:
    value: mac-acceptance
  secret_key:
    value: synthetic-mac-acceptance-secret
encryption:
  enabled: true
  passphrase:
    value: synthetic-mac-acceptance-passphrase
logging:
  format: json
  level: info
''')
        self.daemon = Daemon(BIN / 's3-smb', config,
                             EVIDENCE / f'application-{self.serial}-{phase}.log', phase)
        self.daemon.ready()
        self.event('application-ready', phase=phase, pid=self.daemon.pid)

    def mount_share(self):
        self.share.mkdir(exist_ok=True)
        self.cmd.run(['/sbin/mount_smbfs', '-N', f'//timemachine:{self.password}@{SMB_SERVER}/TimeMachine', self.share])
        self.cmd.run(['/usr/bin/smbutil', 'statshares', '-a'])
        self.cmd.run(['/sbin/mount'])

    def configure_destination(self):
        if self.scenario == 'password-control':
            # Explicit independently labelled positive control, not a fallback
            # after failed empty-password authentication in this run.
            output, _ = self.cmd.run(['/usr/bin/tmutil', 'setdestination',
                                     f'smb://timemachine:{self.password}@{SMB_SERVER}/TimeMachine'])
        else:
            output = self.cmd.set_destination_empty_password(f'smb://timemachine@{SMB_SERVER}/TimeMachine')
        if 'The backup destination could not be set.' in output:
            raise RuntimeError('tmutil reported the backup destination could not be set despite exit 0')
        text, _ = self.cmd.run(['/usr/bin/tmutil', 'destinationinfo', '-X'])
        info = plistlib.loads(text.encode())
        self.save('destination.json', info)
        destinations = info.get('Destinations', [])
        if len(destinations) != 1 or not isinstance(destinations[0].get('ID'), str) or not destinations[0]['ID']:
            raise RuntimeError('tmutil did not configure exactly one Time Machine destination with an ID')
        self.destination = destinations[0]['ID']
        if self.scenario == 'password-control':
            # tmutil authenticated interactively, but backupd's later mount had
            # no usable persisted credential. Inspect only this synthetic
            # account's attributes, then store it via the standard security CLI.
            # No password readback, broad -A ACL, keychain unlock or partition edit.
            keychain = '/Library/Keychains/System.keychain'
            self.cmd.run(['/usr/bin/security', 'find-internet-password', '-s', '127.0.0.1',
                          '-a', 'timemachine', keychain], diagnostic=True)
            attributes = ['-s', '127.0.0.1', '-a', 'timemachine', '-P', str(SMB_PORT),
                          '-r', 'smb ', '-p', 'TimeMachine']
            self.cmd.run(['/usr/bin/security', 'add-internet-password', '-U', *attributes,
                          '-T', '/System/Library/CoreServices/NetAuthAgent.app/Contents/MacOS/NetAuthSysAgent',
                          '-T', '/System/Library/CoreServices/TimeMachine/backupd',
                          '-w', self.password, keychain])
            self.cmd.run(['/usr/bin/security', 'find-internet-password', *attributes, keychain])

    def create_tree(self):
        proof = HOME / 's3-smb-acceptance-proof'
        proof.mkdir(mode=0o700)
        (proof / 'nested/deeper').mkdir(parents=True)
        (proof / 'empty').mkdir()
        (proof / 'nested/empty').mkdir()
        (proof / 'original.bin').write_bytes(os.urandom(4_000_000))
        (proof / 'nested/message.txt').write_text('independent baseline contents\n')
        (proof / 'nested/deeper/zero-length').touch()
        text, _ = self.cmd.run(['/usr/bin/tmutil', 'isexcluded', proof])
        if not text.startswith('[Included]'):
            raise RuntimeError('created tree excluded from normal backup')
        manifest(proof, TRANSFER / 'reference/tree.jsonl')
        self.event('created-tree-reference-saved', path=str(proof))
        return proof

    def observe_task_usage(self):
        if self.task_usage_count >= 2:
            return
        self.task_usage_count += 1
        self.task_bytes = {}
        paths = {str(WORK / 'objects'): 'task_store_bytes', str(self.local): 'task_daemon_bytes',
                 str(EVIDENCE): 'task_evidence_bytes'}
        output, code = self.cmd.run(['/usr/bin/du', '-sk', *paths], timeout=30, diagnostic=True)
        if code == 0:
            for line in output.splitlines():
                size, separator, path = line.partition('\t')
                if separator and size.isdigit() and path in paths:
                    self.task_bytes[paths[path]] = int(size) * 1024
        self.event('task-usage-observed', exit=code)

    def confirm_backup_checkpoint(self):
        self.event('before-first-backup')
        checkpoint = json.loads((EVIDENCE / 'progress.json').read_text())['time']
        deadline = time.monotonic() + 180
        while True:
            try:
                uploaded = json.loads((WORK / 'progress-uploaded.json').read_text())
            except FileNotFoundError:
                uploaded = {}
            if uploaded.get('time') == checkpoint:
                return
            if time.monotonic() >= deadline:
                raise RuntimeError('durable before-first-backup checkpoint was not uploaded within 180 seconds')
            self.daemon.pump()
            time.sleep(.2)

    def start_backup(self, label):
        if self.backup:
            raise RuntimeError('backup already active')
        log = open(EVIDENCE / (label + '-startbackup.log'), 'xb', buffering=0)
        argv = ['/usr/bin/tmutil', 'startbackup', '--block', '--destination', self.destination]
        self.backup = (subprocess.Popen(argv, stdout=log, stderr=subprocess.STDOUT), log)
        self.event('time-machine-start', label=label, argv=argv)

    def status(self):
        text, _ = self.cmd.run(['/usr/bin/tmutil', 'status'])
        if not re.search(r'Running\s*=\s*[01]\s*;', text):
            raise RuntimeError('unknown installed tmutil status format')
        return bool(re.search(r'Running\s*=\s*1\s*;', text))

    def complete_backup(self, label):
        process, log = self.backup
        deadline = time.monotonic() + 5400
        next_observation = time.monotonic()
        usage_observed = False
        while process.poll() is None:
            self.daemon.pump()
            now = time.monotonic()
            if now >= deadline:
                raise RuntimeError('full Time Machine backup exceeded 90 minute stage budget')
            if not usage_observed and now >= deadline - 5400 + 300:
                self.observe_task_usage()
                usage_observed = True
            if now >= next_observation:
                status, code = self.cmd.run(['/usr/bin/tmutil', 'status'], diagnostic=True)
                self.event('time-machine-progress', label=label, native_status=status.strip(), exit=code,
                           **(tm_status_numbers(status) if code == 0 else {}))
                next_observation = now + 60
            time.sleep(1)
        log.close()
        self.backup = None
        if process.returncode or self.status():
            raise RuntimeError(f'Time Machine did not complete cleanly: {process.returncode}')
        completed = utc()
        self.event('time-machine-command-completed', label=label, completed=completed)
        return completed

    def metadata_point(self, after):
        threshold = datetime.datetime.fromisoformat(after)
        deadline = time.monotonic() + 3900  # Native default hourly schedule.
        receipt_path = self.local / 'state/backup-receipt.json'
        next_observation = time.monotonic()
        while time.monotonic() < deadline:
            self.daemon.pump()
            if receipt_path.exists():
                receipt = json.loads(receipt_path.read_text())
                snapshot = datetime.datetime.fromisoformat(receipt['Snapshot'].replace('Z', '+00:00'))
                if snapshot > threshold:
                    self.save('baseline-receipt.json', receipt)
                    self.event('native-point-after-completion', receipt=receipt)
                    return
            if time.monotonic() >= next_observation:
                self.event('waiting-for-native-point', after=after)
                next_observation = time.monotonic() + 60
            time.sleep(1)
        raise RuntimeError('no successful native metadata point after Time Machine completion')

    def detach_clients(self):
        info, _ = self.cmd.run(['/usr/bin/hdiutil', 'info', '-plist'])
        devices = list(self.attachments)
        for image in plistlib.loads(info.encode()).get('images', []):
            path = image.get('image-path', '')
            owned = path.startswith(str(self.share) + '/') or '/127.0.0.1/' in path or f'/{SMB_SERVER}/' in path
            if path.endswith('.sparsebundle') and owned:
                entries = [e['dev-entry'] for e in image.get('system-entities', []) if 'dev-entry' in e]
                if entries and entries[0] not in devices:
                    devices.append(entries[0])
        for device in reversed(devices):
            self.cmd.run(['/usr/bin/hdiutil', 'detach', device], timeout=120)
        self.attachments.clear()
        mounts, _ = self.cmd.run(['/sbin/mount'])
        for line in mounts.splitlines():
            if '(smbfs' in line and f'{SMB_SERVER}/TimeMachine on ' in line:
                mountpoint = line.split(' on ', 1)[1].split(' (', 1)[0]
                self.cmd.run(['/sbin/umount', mountpoint], timeout=120)
        remaining, _ = self.cmd.run(['/sbin/mount'])
        if any('(smbfs' in line and f'{SMB_SERVER}/TimeMachine' in line for line in remaining.splitlines()):
            raise RuntimeError('task SMB mount remains')

    def remote_backup(self, label, identifier=None, inherit=False):
        bundles = sorted(self.share.glob('*.sparsebundle'))
        if len(bundles) != 1:
            raise RuntimeError(f'expected one real Time Machine sparsebundle, got {bundles}')
        if inherit:
            # Documented machine identity reassignment on fresh Mac B; no old
            # daemon authority or machine-local Time Machine state is copied.
            self.cmd.run(['/usr/bin/tmutil', 'inheritbackup', bundles[0]], timeout=300)
        text, _ = self.cmd.run(['/usr/bin/hdiutil', 'attach', '-readonly', '-nobrowse', '-plist', bundles[0]], timeout=300)
        entities = plistlib.loads(text.encode())['system-entities']
        devices = [e['dev-entry'] for e in entities if 'dev-entry' in e]
        self.attachments.append(devices[0])
        volumes = [Path(e['mount-point']) for e in entities if 'mount-point' in e]
        if len(volumes) != 1:
            raise RuntimeError('unknown Time Machine image volume layout')
        volume = volumes[0]
        output, _ = self.cmd.run(['/usr/bin/tmutil', 'listbackups', '-d', volume, '-m'], timeout=120)
        backups = [Path(line) for line in output.splitlines() if line.startswith('/')]
        if identifier is None:
            latest, _ = self.cmd.run(['/usr/bin/tmutil', 'latestbackup', '-d', volume, '-m'], timeout=120)
            selected = Path(latest.strip())
            if selected not in backups:
                raise RuntimeError('latest backup absent from completed remote backup list')
        else:
            matches = [path for path in backups if path.name == identifier]
            if len(matches) != 1:
                raise RuntimeError('completed baseline not present exactly once after recovery')
            selected = matches[0]
        # Retain native device-to-network-image provenance, not a local snapshot.
        info = []
        for path in (selected, volume, Path('/System/Volumes/Data')):
            text, _ = self.cmd.run(['/usr/sbin/diskutil', 'info', '-plist', path])
            info.append(plistlib.loads(text.encode()))
        selected_disk, image_disk, source_disk = info
        parent = image_disk.get('ParentWholeDisk')
        if not parent or selected_disk.get('ParentWholeDisk') != parent or source_disk.get('ParentWholeDisk') == parent:
            raise RuntimeError('selected backup does not belong to remote attached image device')
        if not selected_disk.get('ReadOnlyVolume'):
            raise RuntimeError('selected backup is not read-only')
        self.cmd.run(['/usr/bin/hdiutil', 'info', '-plist'])
        self.cmd.run(['/sbin/mount'])
        self.save(label + '-remote-selection.json', dict(image=str(bundles[0]), device=devices[0],
                  image_volume=str(volume), completed_backups=list(map(str, backups)), selected=str(selected)))
        if not selected.is_dir() or selected.name.endswith('.inProgress'):
            raise RuntimeError('not a completed remote backup directory')
        return selected

    def restore_tree(self, recovery):
        selected = self.remote_backup('normal', recovery['baseline'], inherit=True)
        relative = Path(recovery['source_relative'])
        if relative.is_absolute() or '..' in relative.parts:
            raise RuntimeError('invalid created-tree source path')
        # Inspect volume-root names only, never scan unrelated backed-up trees.
        roots = [path for path in selected.iterdir() if path.name in
                 (recovery['source_volume_name'], 'Data', 'Macintosh HD - Data') and path.is_dir()]
        if len(roots) != 1:
            raise RuntimeError('cannot identify backed-up Data volume')
        source = roots[0] / relative
        if not source.is_dir() or source.is_symlink():
            raise RuntimeError('created tree missing from recovered backup')
        restore = WORK / 'restore'
        if restore.exists():
            raise RuntimeError('restore output must be absent')
        self.event('native-created-tree-restore-start', source=str(source))
        # Reference contains hashes only; no reference file bytes can be copied.
        self.cmd.run(['/usr/bin/tmutil', 'restore', '-v', source, restore], timeout=600, capture=False)
        counts = manifest(restore, EVIDENCE / 'restored-tree.jsonl')
        compare(TRANSFER / 'reference/tree.jsonl', EVIDENCE / 'restored-tree.jsonl', EVIDENCE / 'tree-differences.jsonl')
        self.event('native-created-tree-restore-verified', baseline=recovery['baseline'], **counts)

    def backup_phase(self):
        for directory in ('store', 'reference'):
            (TRANSFER / directory).mkdir(parents=True, exist_ok=True)
        if (WORK / 'objects').exists() or self.local.exists():
            raise RuntimeError('backup requires fresh object store and daemon state')
        self.platform()
        self.start_services(fresh=True)
        self.start_daemon('initialize')
        self.mount_share()
        if list(self.share.iterdir()):
            raise RuntimeError('initial application share not empty')
        self.configure_destination()
        proof = self.create_tree()
        self.observe_task_usage()
        self.confirm_backup_checkpoint()
        self.start_backup('baseline')
        completed = self.complete_backup('baseline')
        # Detach backupd's image before attaching a read-only view.
        self.detach_clients()
        self.mount_share()
        selected = self.remote_backup('baseline')
        text, _ = self.cmd.run(['/usr/sbin/diskutil', 'info', '-plist', '/System/Volumes/Data'])
        recovery = dict(scenario=self.scenario, baseline=selected.name, source_relative=str(proof.relative_to('/')),
                        source_volume_name=plistlib.loads(text.encode())['VolumeName'])
        self.detach_clients()
        self.metadata_point(completed)
        # Archive is deliberately deferred until execute() has cleanly stopped
        # application and both services. Failed cleanup can never ship a store.
        return recovery

    def recover_phase(self):
        if self.local.exists() or (WORK / 'objects').exists():
            raise RuntimeError('recovery requires fresh daemon state and no existing store')
        marker = json.loads((TRANSFER / 'store/store-stopped.json').read_text())
        if marker.get('clean_shutdown') is not True:
            raise RuntimeError('store was not cleanly stopped')
        recovery = json.loads((TRANSFER / 'store/recovery.json').read_text())
        if recovery.get('scenario') != self.scenario:
            raise RuntimeError('recovery scenario does not match the backup; refusing mixed authentication evidence')
        self.platform()
        with tarfile.open(TRANSFER / 'store/minio.tar.gz', 'r:gz') as archive:
            # The only transported payload is stopped MinIO storage. Reject a
            # malformed handoff rather than accepting daemon-local state.
            for member in archive.getmembers():
                path = Path(member.name)
                if path.is_absolute() or '..' in path.parts or path.parts[0] != 'objects' or not (member.isfile() or member.isdir()):
                    raise RuntimeError('unexpected stopped-store archive member')
            archive.extractall(WORK, filter='data')
        self.event('fresh-store-extracted')
        self.start_services(fresh=False)
        self.start_daemon('recover')
        self.mount_share()
        self.restore_tree(recovery)
        self.detach_clients()
        return recovery

    def run(self):
        phase = os.environ['MAC_PHASE']
        if phase == 'backup':
            return self.backup_phase()
        if phase == 'recover':
            return self.recover_phase()
        raise RuntimeError('MAC_PHASE must be backup or recover; crash is not a normal-stage prerequisite')

    def archive_store(self, recovery):
        if self.backup or self.attachments or not self.daemon or not self.daemon.reaped or any(p.poll() is None for p, _ in self.services):
            raise RuntimeError('cannot archive active storage')
        self.event('stopped-store-archive-start')
        with tarfile.open(TRANSFER / 'store/minio.tar.gz', 'x:gz', compresslevel=1) as archive:
            archive.add(WORK / 'objects', arcname='objects')
        (TRANSFER / 'store/recovery.json').write_text(json.dumps(recovery, indent=2) + '\n')
        (TRANSFER / 'store/store-stopped.json').write_text(json.dumps(dict(clean_shutdown=True, time=utc())) + '\n')
        self.event('stopped-store-archive-complete')

    def finish(self):
        errors = []
        outcomes = []

        def attempt(label, action):
            try:
                return action()
            except Exception as error:
                errors.append(f'{label}: {error}')
                return None

        def reap(process, label, service=False):
            initial = process.poll()
            if service and initial is not None:
                errors.append(f'{label} exited before cleanup: {initial}')
            if initial is None:
                attempt(label + ' terminate', process.terminate)
            try:
                code = process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                errors.append(f'{label} exceeded graceful shutdown deadline')
                attempt(label + ' kill', process.kill)
                code = attempt(label + ' reap after kill', lambda: process.wait(timeout=5))
            outcomes.append(dict(process=label, pid=process.pid, exit=code))
            if service and code != 0:
                errors.append(f'{label} unsuccessful exit: {code}')

        if self.backup:
            errors.append('owned Time Machine startbackup still active at final cleanup')
            process, log = self.backup
            attempt('Time Machine stopbackup', lambda: self.cmd.run(['/usr/bin/tmutil', 'stopbackup'], timeout=60))
            attempt('Time Machine client reap', lambda: reap(process, 'startbackup'))
            attempt('Time Machine client log close', log.close)
            self.backup = None
        if self.daemon and not self.daemon.reaped:
            attempt('detach clients', self.detach_clients)
            attempt('application stop', self.daemon.stop)
            outcomes.append(dict(process='application', pid=self.daemon.pid,
                                 reaped=self.daemon.reaped, status=self.daemon.exit_status))
        for process, log in reversed(self.services):
            attempt('service reap', lambda p=process: reap(p, str(p.args[0]), service=True))
            attempt('service log close', log.close)
        for argv in (['/usr/bin/tmutil', 'status'], ['/sbin/mount'],
                     ['/usr/bin/hdiutil', 'info', '-plist'], ['/bin/df', '-k'],
                     ['/usr/bin/log', 'show', '--style', 'json', '--last', '6h', '--info', '--debug',
                      '--predicate', 'process == "backupd" OR process == "backupd-helper" OR process == "tmutil" OR process == "NetAuthSysAgent" OR subsystem BEGINSWITH "com.apple.smb"']):
            try:
                self.cmd.run(argv, timeout=90, diagnostic=True, capture=False)
            except Exception as error:
                attempt('diagnostic evidence', lambda: self.event('diagnostic-failed', error=str(error)))
        attempt('cleanup evidence', lambda: self.save('cleanup.json', dict(outcomes=outcomes, errors=errors.copy())))
        if errors:
            raise RuntimeError('cleanup failed: ' + '; '.join(errors))

    def execute(self):
        failures = []
        result = None
        try:
            result = self.run()
        except BaseException:
            failures.append(traceback.format_exc())
        finally:
            try:
                self.finish()
            except BaseException:
                failures.append(traceback.format_exc())
        if not failures and os.environ.get('MAC_PHASE') == 'backup':
            try:
                self.archive_store(result)
            except BaseException:
                failures.append(traceback.format_exc())
        if failures:
            (EVIDENCE / 'failure.txt').write_text('\n'.join(failures))
            self.event('acceptance-failed', failures=len(failures))
            raise RuntimeError('acceptance failed; see failure.txt and cleanup.json')
        self.event('acceptance-passed', phase=os.environ.get('MAC_PHASE'), **result)


if __name__ == '__main__':
    def deadline_reached(signum, frame):
        raise TimeoutError('acceptance deadline reached; remaining stages are NOT passed')
    signal.signal(signal.SIGALRM, deadline_reached)
    signal.alarm(max(1, int(os.environ['MAC_DEADLINE_EPOCH']) - int(time.time())))
    Acceptance().execute()
