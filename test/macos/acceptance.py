#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Time Machine backup, recovery on a second Mac, and the failed-backup scenarios."""
import datetime
import json
import os
from pathlib import Path
import plistlib
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time
import traceback
import urllib.error
import urllib.request

from manifest import manifest, compare
from native import Commands, Daemon, utc, tm_status_numbers

WORK = Path(os.environ['MAC_WORK']).resolve()
EVIDENCE = Path(os.environ['MAC_ARTIFACTS']).resolve()
HOME = Path(os.environ['MAC_RUNNER_HOME']).resolve()
BIN = Path(os.environ['MAC_BIN']).resolve()
TRANSFER = Path(os.environ.get('MAC_TRANSFER', str(WORK / 'transfer'))).resolve()
# Apple SMBClient explicitly permits local servers on nonstandard SMB ports;
# its NetFS path rejects local 139/445 by default (unlike mount_smbfs).
SMB_PORT = 1445
SMB_SERVER = f'127.0.0.1:{SMB_PORT}'
PROOF = HOME / 's3-smb-acceptance-proof'
# Every directory next to the tree and next to its ancestors, as printed by the
# discover run. The ancestors /System/Volumes/Data, Users and runner stay in the
# backup, and so do loose files next to the tree.
# tmutil addexclusion -p excludes whatever is at that path, even after it is
# deleted and recreated. Without -p the exclusion follows the item when it moves
# and copies keep it. -v excludes a whole volume. An excluded directory excludes
# everything under it, and removing a child's own exclusion does not include
# that child again.
DATA = '/System/Volumes/Data'
EXCLUSIONS = [
    f'{DATA}/.Spotlight-V100', f'{DATA}/.TemporaryItems', f'{DATA}/.fseventsd',
    f'{DATA}/Applications', f'{DATA}/Library', f'{DATA}/MobileSoftwareUpdate',
    f'{DATA}/Previous Content', f'{DATA}/System', f'{DATA}/Volumes',
    f'{DATA}/cores', f'{DATA}/mnt', f'{DATA}/opt', f'{DATA}/private',
    f'{DATA}/sw', f'{DATA}/usr',
    '/Users/Shared',
    '/Users/runner/.Azure', '/Users/runner/.Trash', '/Users/runner/.android',
    '/Users/runner/.azure-devops', '/Users/runner/.cache', '/Users/runner/.cargo',
    '/Users/runner/.config', '/Users/runner/.dotnet', '/Users/runner/.gradle',
    '/Users/runner/.homebrew', '/Users/runner/.local', '/Users/runner/.net',
    '/Users/runner/.npm', '/Users/runner/.rustup', '/Users/runner/.ssh',
    '/Users/runner/.vcpkg', '/Users/runner/.yarn', '/Users/runner/Desktop',
    '/Users/runner/Documents', '/Users/runner/Downloads', '/Users/runner/Library',
    '/Users/runner/Movies', '/Users/runner/actionarchivecache',
    '/Users/runner/actions-runner', '/Users/runner/bootstrap',
    '/Users/runner/go', '/Users/runner/hostedtoolcache',
    '/Users/runner/image-generation', '/Users/runner/work',
]


class Acceptance:
    def __init__(self):
        # Public synthetic loopback fixture only.
        self.password = 'synthetic-tm-control'
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
        self.interval = '5m'

    def event(self, event, **fields):
        with (EVIDENCE / 'acceptance.jsonl').open('a') as f:
            f.write(json.dumps(dict(time=utc(), event=event, **fields), sort_keys=True) + '\n')
        print(event, fields, flush=True)

    def save(self, name, data):
        with (EVIDENCE / name).open('x') as f:
            json.dump(data, f, indent=2, sort_keys=True)

    def platform(self):
        if sys.platform != 'darwin' or os.geteuid() != 0:
            raise RuntimeError('native Darwin administrative execution required')
        self.status()
        self.cmd.run(['/sbin/mount'])
        # The hosted runner image runs tmutil disable and unloads backupd.
        _, code = self.cmd.run(['/bin/launchctl', 'print', 'system/com.apple.backupd'], diagnostic=True)
        if code:
            self.cmd.run(['/bin/launchctl', 'enable', 'system/com.apple.backupd'])
            self.cmd.run(['/bin/launchctl', 'bootstrap', 'system', '/System/Library/LaunchDaemons/com.apple.backupd.plist'])
        self.cmd.run(['/bin/launchctl', 'print', 'system/com.apple.backupd'])
        paths = (WORK, EVIDENCE, TRANSFER)
        self.save('test-exclusions.json', [dict(path=str(p), reason='task infrastructure; prevent recursive backup') for p in paths])
        for path in paths:
            self.cmd.run(['/usr/bin/tmutil', 'addexclusion', '-p', path])
        if os.environ['MAC_PHASE'] != 'recover':
            self.apply_exclusions()
        self.cmd.run(['/bin/df', '-k'])
        self.event('platform-ready')

    def apply_exclusions(self, diagnostic=False):
        for path in EXCLUSIONS:
            if not os.path.lexists(path):
                print('exclusion-absent', shlex.quote(path), flush=True)
                continue
            _, code = self.cmd.run(['/usr/bin/tmutil', 'addexclusion', '-p', path], diagnostic=diagnostic)
            print('exclusion-added', shlex.quote(path), f'exit={code}', flush=True)

    def isexcluded(self, path):
        text, _ = self.cmd.run(['/usr/bin/tmutil', 'isexcluded', path])
        print(text.strip(), flush=True)
        return text

    def check_exclusions(self):
        wrong = []
        for path in (PROOF, PROOF / 'nested/message.txt', PROOF / 'empty'):
            if not self.isexcluded(path).startswith('[Included]'):
                wrong.append(str(path))
        for path in (WORK / 'objects', '/Users/runner/Library'):
            if not self.isexcluded(path).startswith('[Excluded]'):
                wrong.append(str(path))
        if wrong:
            raise RuntimeError(f'wrong Time Machine exclusion state: {wrong}')

    def service(self, argv, name):
        log = open(EVIDENCE / (name + '.log'), 'xb', buffering=0)
        process = subprocess.Popen([str(x) for x in argv], stdout=log, stderr=subprocess.STDOUT)
        self.services.append((process, log))
        return process

    def start_services(self, fresh):
        import socket
        for port in (SMB_PORT, 19000, 19003):
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
            if self.objects(''):
                raise RuntimeError('initial bucket is not empty')

    def objects(self, prefix):
        output, _ = self.cmd.run([self.fixture, 'bucket-list', '--endpoint', 'http://127.0.0.1:19000',
                                  '--bucket', 'time-machine', '--prefix', prefix], timeout=300)
        return {o['key']: o['size'] for o in json.loads(output)['objects']}

    def start_daemon(self, phase):
        self.serial += 1
        config = self.local / 'config.yaml'
        # A restart keeps the directory and the config of the killed process.
        if phase != 'restart':
            self.local.mkdir(mode=0o700)
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
  endpoint: http://127.0.0.1:19000
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
backup:
  interval: {self.interval}
logging:
  format: json
  level: info
''')
        self.daemon = Daemon(BIN / 's3-smb', config,
                             EVIDENCE / f'application-{self.serial}-{phase}.log', phase)
        self.daemon.ready()
        self.event('application-ready', phase=phase, pid=self.daemon.pid, recovered_from=self.daemon.point)

    def mount_share(self):
        self.share.mkdir(exist_ok=True)
        self.cmd.run(['/sbin/mount_smbfs', '-N', f'//timemachine:{self.password}@{SMB_SERVER}/TimeMachine', self.share])
        self.cmd.run(['/usr/bin/smbutil', 'statshares', '-a'])
        self.cmd.run(['/sbin/mount'])

    def configure_destination(self):
        output, _ = self.cmd.run(['/usr/bin/tmutil', 'setdestination',
                                 f'smb://timemachine:{self.password}@{SMB_SERVER}/TimeMachine'])
        if 'The backup destination could not be set.' in output:
            raise RuntimeError('tmutil reported the backup destination could not be set despite exit 0')
        text, _ = self.cmd.run(['/usr/bin/tmutil', 'destinationinfo', '-X'])
        info = plistlib.loads(text.encode())
        self.save('destination.json', info)
        destinations = info.get('Destinations', [])
        if len(destinations) != 1 or not isinstance(destinations[0].get('ID'), str) or not destinations[0]['ID']:
            raise RuntimeError('tmutil did not configure exactly one Time Machine destination with an ID')
        self.destination = destinations[0]['ID']
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
        proof = PROOF
        proof.mkdir(mode=0o700)
        (proof / 'nested/deeper').mkdir(parents=True)
        (proof / 'empty').mkdir()
        (proof / 'nested/empty').mkdir()
        (proof / 'original.bin').write_bytes(os.urandom(4_000_000))
        (proof / 'nested/message.txt').write_text('independent baseline contents\n')
        (proof / 'nested/deeper/zero-length').touch()
        (TRANSFER / 'reference').mkdir()
        manifest(proof, TRANSFER / 'reference/tree.jsonl')
        self.event('created-tree-reference-saved', path=str(proof))
        return proof

    def start_backup(self, label):
        if self.backup:
            raise RuntimeError('backup already active')
        log = open(EVIDENCE / (label + '-startbackup.log'), 'xb', buffering=0)
        argv = ['/usr/bin/tmutil', 'startbackup', '--block', '--destination', self.destination]
        self.backup = (subprocess.Popen(argv, stdout=log, stderr=subprocess.STDOUT), log)
        self.backup_started = time.monotonic()
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
        while process.poll() is None:
            self.daemon.pump()
            now = time.monotonic()
            if now >= deadline:
                raise RuntimeError('full Time Machine backup exceeded 90 minute stage budget')
            if now >= next_observation:
                status, code = self.cmd.run(['/usr/bin/tmutil', 'status'], diagnostic=True)
                free = shutil.disk_usage(WORK).free
                self.event('time-machine-progress', label=label, native_status=status.strip(), exit=code,
                           free_bytes=free, **(tm_status_numbers(status) if code == 0 else {}))
                if free < 20 * 2**30:
                    raise RuntimeError('free space below 20 GiB')
                next_observation = now + 60
            time.sleep(1)
        log.close()
        self.backup = None
        if process.returncode or self.status():
            raise RuntimeError(f'Time Machine did not complete cleanly: {process.returncode}')
        completed = utc()
        # The application has closed Time Machine's connection with this message
        # during some backups. Count it in every run.
        short_headers = sum(path.read_bytes().count(b'short client packet header')
                            for path in EVIDENCE.glob('application-*.log'))
        self.event('time-machine-command-completed', label=label, completed=completed,
                   seconds=round(time.monotonic() - self.backup_started), short_headers=short_headers)
        return completed

    def metadata_point(self, after, label):
        """Wait for a metadata backup taken after the given time and return its receipt."""
        threshold = datetime.datetime.fromisoformat(after)
        deadline = time.monotonic() + 900  # The harness config backs up every 5 minutes.
        receipt_path = self.local / 'state/backup-receipt.json'
        next_observation = time.monotonic()
        while time.monotonic() < deadline:
            self.daemon.pump()
            if receipt_path.exists():
                receipt = json.loads(receipt_path.read_text())
                # Go writes up to nine fraction digits and drops trailing zeros.
                whole, _, fraction = receipt['Snapshot'].rstrip('Z').partition('.')
                snapshot = datetime.datetime.strptime(whole, '%Y-%m-%dT%H:%M:%S').replace(
                    microsecond=int(fraction.ljust(6, '0')[:6]), tzinfo=datetime.timezone.utc)
                if snapshot > threshold:
                    self.save(label + '-receipt.json', receipt)
                    # The application writes the receipt when the upload has ended.
                    written = datetime.datetime.fromtimestamp(receipt_path.stat().st_mtime, datetime.timezone.utc)
                    self.event('native-point-after-completion', label=label, receipt=receipt,
                               object_bytes=self.objects('s3-smb/meta/')['s3-smb/' + receipt['Key']],
                               seconds=round((written - snapshot).total_seconds(), 1))
                    return receipt
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
        # tmutil mounts each backup it lists as an APFS snapshot of the image
        # volume. The image cannot be detached while one is mounted.
        mounts, _ = self.cmd.run(['/sbin/mount'])
        for line in mounts.splitlines():
            if line.startswith('com.apple.TimeMachine.') and ' on /Volumes/.timemachine/' in line:
                self.cmd.run(['/sbin/umount', line.split(' on ', 1)[1].split(' (', 1)[0]], timeout=120, diagnostic=True)
        for device in reversed(devices):
            _, code = self.cmd.run(['/usr/bin/hdiutil', 'detach', device], timeout=120, diagnostic=True)
            # startbackup can return while backupd is still ejecting its image.
            deadline = time.monotonic() + 60
            while code:
                info, _ = self.cmd.run(['/usr/bin/hdiutil', 'info', '-plist'])
                if device not in [e.get('dev-entry') for i in plistlib.loads(info.encode()).get('images', [])
                                  for e in i.get('system-entities', [])]:
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError(f'cannot detach {device}')
                time.sleep(5)
                _, code = self.cmd.run(['/usr/bin/hdiutil', 'detach', '-force', device], timeout=120, diagnostic=True)
        self.attachments.clear()
        # backupd can still be ejecting its mount after a completed backup.
        deadline = time.monotonic() + 60
        while True:
            mounts, _ = self.cmd.run(['/sbin/mount'])
            mountpoints = [line.split(' on ', 1)[1].split(' (', 1)[0] for line in mounts.splitlines()
                           if '(smbfs' in line and f'{SMB_SERVER}/TimeMachine on ' in line]
            if not mountpoints:
                return
            late = time.monotonic() >= deadline
            for mountpoint in mountpoints:
                _, code = self.cmd.run(['/sbin/umount', mountpoint], timeout=120, diagnostic=True)
                if code and late:
                    self.cmd.run(['/sbin/umount', '-f', mountpoint], timeout=120)
            if late:
                break
            time.sleep(5)
        remaining, _ = self.cmd.run(['/sbin/mount'])
        if any('(smbfs' in line and f'{SMB_SERVER}/TimeMachine' in line for line in remaining.splitlines()):
            raise RuntimeError('task SMB mount remains')

    def attach(self, bundle, *flags):
        text, code = self.cmd.run(['/usr/bin/hdiutil', 'attach', *flags, '-nobrowse', '-plist', bundle],
                                  timeout=300, diagnostic=True)
        entities = plistlib.loads(text.encode())['system-entities'] if code == 0 else []
        return ([e['dev-entry'] for e in entities if 'dev-entry' in e],
                [Path(e['mount-point']) for e in entities if 'mount-point' in e])

    def remote_backup(self, label, identifier=None):
        bundles = sorted(self.share.glob('*.sparsebundle'))
        if len(bundles) != 1:
            raise RuntimeError(f'expected one real Time Machine sparsebundle, got {bundles}')
        devices, volumes = self.attach(bundles[0], '-readonly')
        if len(volumes) != 1:
            # An interrupted backup leaves the volume in the image unclean, and a
            # read-only attach cannot replay it. Attach it writable once.
            for device in devices[:1]:
                self.cmd.run(['/usr/bin/hdiutil', 'detach', '-force', device], timeout=120)
            devices, _ = self.attach(bundles[0])
            self.cmd.run(['/usr/bin/hdiutil', 'detach', devices[0]], timeout=300)
            devices, volumes = self.attach(bundles[0], '-readonly')
        self.attachments.append(devices[0])
        if len(volumes) != 1:
            raise RuntimeError('unknown Time Machine image volume layout')
        volume = volumes[0]
        # backupd answers only after Spotlight has indexed the volume. That took 90 seconds.
        # The second Mac did not make this backup, so ask without -m when -m lists nothing.
        for flags in (['-m'], []):
            output, _ = self.cmd.run(['/usr/bin/tmutil', 'listbackups', '-d', volume, *flags], timeout=600)
            backups = [Path(line) for line in output.splitlines() if line.startswith('/')]
            if backups:
                break
        if identifier is None:
            latest, _ = self.cmd.run(['/usr/bin/tmutil', 'latestbackup', '-d', volume, '-m'], timeout=600)
            selected = Path(latest.strip())
            if selected not in backups:
                raise RuntimeError('latest backup absent from completed remote backup list')
        else:
            matches = [path for path in backups if path.name == identifier]
            if len(matches) != 1:
                raise RuntimeError('completed baseline not present exactly once after recovery')
            selected = matches[0]
        self.cmd.run(['/usr/bin/hdiutil', 'info', '-plist'])
        self.cmd.run(['/sbin/mount'])
        self.save(label + '-remote-selection.json', dict(image=str(bundles[0]), device=devices[0],
                  image_volume=str(volume), completed_backups=list(map(str, backups)), selected=str(selected)))
        if not selected.is_dir() or selected.name.endswith('.inProgress'):
            raise RuntimeError('not a completed remote backup directory')
        return selected

    def tree_in_backup(self, selected, relative):
        # A backup has one folder per source volume. Take the one that holds the tree.
        volumes = sorted(selected.iterdir())
        found = [path / relative for path in volumes
                 if (path / relative).is_dir() and not (path / relative).is_symlink()]
        if len(found) != 1:
            raise RuntimeError(f'created tree is not in exactly one volume of the backup: {[p.name for p in volumes]}')
        return found[0]

    def restore_tree(self, selected, reference, output_dir):
        source = self.tree_in_backup(selected, PROOF.relative_to('/'))
        if output_dir.exists():
            raise RuntimeError('restore output must be absent')
        self.event('native-created-tree-restore-start', source=str(source))
        # Reference contains hashes only; no reference file bytes can be copied.
        self.cmd.run(['/usr/bin/tmutil', 'restore', '-v', source, output_dir], timeout=1800, capture=False)
        counts = manifest(output_dir, EVIDENCE / f'{output_dir.name}.jsonl')
        compare(reference, EVIDENCE / f'{output_dir.name}.jsonl', EVIDENCE / f'{output_dir.name}-differences.jsonl')
        self.event('native-created-tree-restore-verified', backup=selected.name, **counts)
        return counts

    def backup_phase(self):
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
        self.check_exclusions()
        self.start_backup('baseline')
        completed = self.complete_backup('baseline')
        # Detach backupd's image before attaching a read-only view.
        self.detach_clients()
        self.mount_share()
        selected = self.remote_backup('baseline')
        self.tree_in_backup(selected, proof.relative_to('/'))
        recovery = dict(baseline=selected.name)
        self.detach_clients()
        self.metadata_point(completed, 'baseline')
        # Export is deliberately deferred until execute() has cleanly stopped
        # application and both services. Failed cleanup can never ship a store.
        return recovery

    def wait_copying(self):
        """Start the later backup and return tmutil's status once it has copied bytes."""
        self.start_backup('interrupted')
        deadline = time.monotonic() + 1800
        while time.monotonic() < deadline:
            self.daemon.pump()
            if self.backup[0].poll() is not None:
                raise RuntimeError('Time Machine ended before Copying was seen')
            text, _ = self.cmd.run(['/usr/bin/tmutil', 'status'])
            if re.search(r'BackupPhase\s*=\s*Copying', text) and tm_status_numbers(text).get('tm_bytes', 0) > 0:
                self.event('time-machine-copying', native_status=text.strip())
                return text.strip()
            time.sleep(2)
        raise RuntimeError('Time Machine did not reach Copying in 30 minutes')

    def stop_backup_client(self):
        process, log = self.backup
        self.cmd.run(['/usr/bin/tmutil', 'stopbackup'], timeout=60, diagnostic=True)
        # backupd can take minutes to give up on a server that is gone.
        try:
            process.wait(timeout=180)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        log.close()
        self.backup = None
        deadline = time.monotonic() + 180
        while self.status() and time.monotonic() < deadline:
            time.sleep(5)
        self.detach_clients()

    def cold_recover(self):
        """Delete the application's database, cache and config, then recover from S3."""
        if not self.daemon.reaped:
            self.daemon.stop()
        shutil.rmtree(self.local)
        self.start_daemon('recover')

    def server_kill_restart(self, p1):
        at_kill = self.wait_copying()
        self.daemon.stop(abrupt=True)
        self.stop_backup_client()
        self.start_daemon('restart')
        return at_kill

    def server_kill_cold(self, p1):
        at_kill = self.wait_copying()
        self.daemon.stop(abrupt=True)
        self.stop_backup_client()
        self.cold_recover()
        if self.daemon.point != p1['Key']:
            raise RuntimeError(f'recovered from {self.daemon.point}, expected {p1["Key"]} from before the later backup')
        return at_kill

    def server_kill_cold_midpoint(self, p1):
        self.wait_copying()
        p2 = self.metadata_point(utc(), 'midpoint')
        at_kill, _ = self.cmd.run(['/usr/bin/tmutil', 'status'])
        if not re.search(r'BackupPhase\s*=\s*Copying', at_kill):
            raise RuntimeError('Time Machine stopped copying before the metadata backup landed')
        self.daemon.stop(abrupt=True)
        self.stop_backup_client()
        self.cold_recover()
        if self.daemon.point < p2['Key']:
            raise RuntimeError(f'recovered from {self.daemon.point}, expected {p2["Key"]} or later')
        return at_kill.strip()

    def client_abort_cold(self, p1):
        at_kill = self.wait_copying()
        aborted = utc()
        self.cmd.run(['/usr/bin/tmutil', 'stopbackup'], timeout=60, diagnostic=True)
        self.cmd.run(['/usr/bin/pkill', '-9', '-x', 'backupd'], diagnostic=True)
        mounts, _ = self.cmd.run(['/sbin/mount'])
        for line in mounts.splitlines():
            if '(smbfs' in line and f'{SMB_SERVER}/TimeMachine on ' in line:
                self.cmd.run(['/sbin/umount', '-f', line.split(' on ', 1)[1].split(' (', 1)[0]], timeout=120, diagnostic=True)
        self.stop_backup_client()
        after = self.metadata_point(aborted, 'after-abort')
        self.cold_recover()
        if self.daemon.point < after['Key']:
            raise RuntimeError(f'recovered from {self.daemon.point}, expected {after["Key"]} or later')
        return at_kill

    def machine_loss(self, p1):
        at_kill = self.wait_copying()
        self.daemon.stop(abrupt=True)
        self.stop_backup_client()
        return at_kill

    def scenario_phase(self, name):
        middle = {'server-kill-restart': self.server_kill_restart, 'server-kill-cold': self.server_kill_cold,
                  'server-kill-cold-midpoint': self.server_kill_cold_midpoint,
                  'client-abort-cold': self.client_abort_cold, 'machine-loss': self.machine_loss}[name]
        # A metadata backup must land while Time Machine is still copying.
        midpoint = name == 'server-kill-cold-midpoint'
        if midpoint:
            self.interval = '1m'
        self.platform()
        self.start_services(fresh=True)
        self.start_daemon('initialize')
        self.mount_share()
        self.configure_destination()
        proof = self.create_tree()
        self.check_exclusions()
        self.start_backup('baseline')
        self.complete_backup('baseline')
        self.detach_clients()
        self.mount_share()
        baseline = self.remote_backup('baseline').name
        self.detach_clients()
        with (proof / 'later.bin').open('wb') as later:
            for _ in range(64 if midpoint else 16):
                later.write(os.urandom(64 * 2**20))
        (proof / 'nested/message.txt').write_text('changed after the baseline\n')
        manifest(proof, EVIDENCE / 'updated-tree.jsonl')
        # Take the metadata backup last, so that the kill comes before the next one.
        p1 = self.metadata_point(utc(), 'baseline')
        chunks = [len(self.objects('s3-smb/chunks/'))]
        result = dict(scenario=name, baseline=baseline, metadata_backup=p1['Key'], at_kill=middle(p1))
        if name == 'machine-loss':
            # The second Mac recovers and restores. execute() exports the store.
            return result
        chunks.append(len(self.objects('s3-smb/chunks/')))
        self.mount_share()
        result['baseline_restore'] = self.restore_tree(
            self.remote_backup('baseline-recovered', baseline), TRANSFER / 'reference/tree.jsonl', WORK / 'restore-baseline')
        self.detach_clients()
        # Time Machine may need a second start after an interrupted backup.
        for label in ('resumed', 'resumed-retry'):
            self.start_backup(label)
            try:
                self.complete_backup(label)
            except RuntimeError as error:
                self.event('resumed-backup-failed', label=label, error=str(error))
            self.detach_clients()
            self.mount_share()
            latest = self.remote_backup(label)
            if latest.name != baseline:
                break
            self.detach_clients()
        else:
            raise RuntimeError('no backup completed after recovery')
        result['resumed_restore'] = self.restore_tree(latest, EVIDENCE / 'updated-tree.jsonl', WORK / 'restore-resumed')
        self.detach_clients()
        chunks.append(len(self.objects('s3-smb/chunks/')))
        return dict(result, resumed=latest.name, recovered_from=self.daemon.point, chunk_objects=chunks)

    def discover_phase(self):
        if sys.platform != 'darwin' or os.geteuid() != 0:
            raise RuntimeError('native Darwin administrative execution required')
        found = []
        for parent, kept in ((DATA, 'Users'), ('/Users', HOME.name), (str(HOME), PROOF.name)):
            with os.scandir(parent) as entries:
                for entry in sorted(entries, key=lambda e: e.name):
                    if entry.name == kept or not entry.is_dir(follow_symlinks=False) or os.path.ismount(entry.path):
                        continue
                    if '\r' in entry.name or '\n' in entry.name:
                        print('discover-skipped', repr(entry.path), flush=True)
                        continue
                    found.append(entry.path)
        print('discover-directories')
        for path in found:
            print(shlex.quote(path))
        print('EXCLUSIONS = [')
        for path in found:
            print(f'    {path!r},')
        print(']')
        print('discover-not-in-list', sorted(set(found) - set(EXCLUSIONS)))
        print('discover-list-only', sorted(set(EXCLUSIONS) - set(found)))
        print('discover-image', os.environ.get('MAC_IMAGE'), flush=True)
        for argv in (['/usr/bin/sw_vers'], ['/sbin/mount']):
            text, _ = self.cmd.run(argv)
            print(text, flush=True)
        self.create_tree()
        (WORK / 'objects').mkdir()
        self.apply_exclusions(diagnostic=True)
        for name in ('Applications', 'Library', 'opt'):
            self.isexcluded(f'/{name}')
            self.isexcluded(f'{DATA}/{name}')
        self.check_exclusions()
        return {}

    def recover_phase(self):
        if self.local.exists() or self.local.is_symlink() or (WORK / 'objects').exists() or (WORK / 'objects').is_symlink():
            raise RuntimeError('recovery requires fresh daemon state and no existing store')
        recovery = json.loads((TRANSFER / 'reference/recovery.json').read_text())
        self.platform()
        tar = TRANSFER / 'store.tar'
        self.cmd.run(['/usr/bin/tar', '-C', WORK, '-xf', tar], timeout=1800)
        tar.unlink()
        self.event('fresh-store-received')
        self.start_services(fresh=False)
        self.start_daemon('recover')
        # After a lost machine the newest metadata backup is the one from before the failed backup.
        if recovery.get('metadata_backup', self.daemon.point) != self.daemon.point:
            raise RuntimeError(f'recovered from {self.daemon.point}, expected {recovery["metadata_backup"]}')
        self.mount_share()
        self.restore_tree(self.remote_backup('baseline', recovery['baseline']),
                          TRANSFER / 'reference/tree.jsonl', WORK / 'restore-baseline')
        self.detach_clients()
        return dict(recovery, recovered_from=self.daemon.point)

    def run(self):
        phase = os.environ['MAC_PHASE']
        if phase == 'backup':
            return self.backup_phase()
        if phase == 'recover':
            return self.recover_phase()
        if phase == 'discover':
            return self.discover_phase()
        if phase == 'scenario':
            return self.scenario_phase(os.environ['MAC_SCENARIO'])
        raise RuntimeError('MAC_PHASE must be discover, backup, recover or scenario')

    def export_store(self, recovery):
        if self.backup or self.attachments or not self.daemon or not self.daemon.reaped or any(p.poll() is None for p, _ in self.services):
            raise RuntimeError('cannot export active storage')
        output, _ = self.cmd.run(['/usr/bin/du', '-sk', WORK / 'objects'], timeout=600)
        self.event('stopped-store-export-start', store_kilobytes=int(output.split()[0]))
        self.cmd.run(['/usr/bin/tar', '-C', WORK, '-cf', TRANSFER / 'store.tar', 'objects'], timeout=1800)
        (TRANSFER / 'reference/recovery.json').write_text(json.dumps(recovery, indent=2) + '\n')
        self.event('stopped-store-export-complete')

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
                      '--predicate', 'process == "backupd" OR process == "backupd-helper" OR process == "tmutil" OR process == "NetAuthSysAgent" OR subsystem BEGINSWITH "com.apple.smb" OR senderImagePath CONTAINS "smbfs"']):
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
        if not failures and (os.environ['MAC_PHASE'] == 'backup' or os.environ.get('MAC_SCENARIO') == 'machine-loss'):
            try:
                self.export_store(result)
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
