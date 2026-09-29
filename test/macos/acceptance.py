#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Final native acceptance. A failure is a failure, never an alternate fixture test."""
import datetime
import json
import os
from pathlib import Path
import plistlib
import re
import shutil
import signal
import subprocess
import sys
import time
import traceback
import urllib.request

from manifest import manifest, compare
from native import Commands, Daemon, capacity_requirement, eligible_source, utc

WORK = Path(os.environ['MAC_WORK']).resolve()
EVIDENCE = Path(os.environ['MAC_ARTIFACTS']).resolve()
HOME = Path(os.environ['MAC_RUNNER_HOME']).resolve()
BIN = Path(os.environ['MAC_BIN']).resolve()
CHANGE_BYTES = 512_000_000


class Acceptance:
    def __init__(self):
        self.cmd = Commands(EVIDENCE)
        self.daemon = None
        self.services = []
        self.backup = None
        self.attachments = []
        self.share = WORK / 'smb'
        self.local = WORK / 'daemon'
        self.fixture = BIN / 'fixture'
        self.started = utc()
        self.serial = 0
        self.baseline = None
        self.destination = None
        self.source_volume_name = None

    def event(self, event, **fields):
        with (EVIDENCE / 'acceptance.jsonl').open('a') as f:
            f.write(json.dumps(dict(time=utc(), event=event, **fields), sort_keys=True) + '\n')
        print(event, fields, flush=True)

    def save(self, name, data):
        with (EVIDENCE / name).open('x') as f:
            json.dump(data, f, indent=2, sort_keys=True)

    def control(self, verb='state', method='GET'):
        request = urllib.request.Request('http://127.0.0.1:19002/' + verb, method=method,
                                         data=b'' if method == 'POST' else None)
        with urllib.request.urlopen(request, timeout=15) as r:
            state = json.load(r)
        if not state['evidence_ok']:
            raise RuntimeError('fixture evidence writer failed')
        return state

    def platform(self):
        if sys.platform != 'darwin' or os.geteuid() != 0:
            raise RuntimeError('native Darwin administrative execution required')
        # Establish actual installed CLI, not a remembered internet man page.
        manual, _ = self.cmd.run(['/bin/sh', '-c', 'MANPAGER=cat MANWIDTH=160 man tmutil | col -b'])
        required = {
            'setdestination': ['smb'], 'destinationinfo': ['-X'],
            'startbackup': ['--block', '--destination'], 'stopbackup': [],
            'listbackups': ['-d'], 'latestbackup': ['-d'],
            'restore': ['-v'], 'isexcluded': [], 'addexclusion': ['-p'],
            'status': [],
        }
        for verb, options in required.items():
            help_text, code = self.cmd.run(['/usr/bin/tmutil', 'help', verb], diagnostic=True)
            # Help may intentionally return usage status; require actual verb and
            # option contract in its output. Never use this for acceptance work.
            if code not in (0, 1) or verb not in help_text or any(x not in help_text for x in options):
                raise RuntimeError(f'installed tmutil help does not establish {verb} {options}')
        if 'restore' not in manual:
            raise RuntimeError('installed tmutil manual missing restore contract')
        self.cmd.run(['/sbin/mount'])
        source_info, _ = self.cmd.run(['/usr/sbin/diskutil', 'info', '-plist', '/System/Volumes/Data'])
        self.source_volume_name = plistlib.loads(source_info.encode())['VolumeName']
        self.cmd.run(['/bin/launchctl', 'print-disabled', 'system'])
        service, code = self.cmd.run(['/bin/launchctl', 'print', 'system/com.apple.backupd'], diagnostic=True)
        if code:
            # Supported administration of the existing Apple daemon, never TCC,
            # SIP, entitlement, privacy database or permission-control changes.
            self.cmd.run(['/bin/launchctl', 'help', 'enable'])
            self.cmd.run(['/bin/launchctl', 'help', 'bootstrap'])
            self.cmd.run(['/bin/launchctl', 'enable', 'system/com.apple.backupd'])
            self.cmd.run(['/bin/launchctl', 'bootstrap', 'system', '/System/Library/LaunchDaemons/com.apple.backupd.plist'])
        self.cmd.run(['/bin/launchctl', 'print', 'system/com.apple.backupd'])
        # Only recursion-producing test state/evidence. Builds, public install
        # caches, SDKs, source checkout and user content remain untouched.
        self.save('test-exclusions.json', [
            dict(path=str(WORK), reason='only backend objects, daemon authority/cache/config, network mount and sequential restore: avoid recursive self-backup'),
            dict(path=str(EVIDENCE), reason='growing command logs/manifests of this backup: avoid recursive self-backup')])
        for path in (WORK, EVIDENCE):
            self.cmd.run(['/usr/bin/tmutil', 'addexclusion', '-p', str(path)])
        self.cmd.run(['/usr/bin/tmutil', 'isexcluded', str(WORK), str(EVIDENCE), str(HOME)])
        self.cmd.run(['/usr/bin/defaults', 'read', '/Library/Preferences/com.apple.TimeMachine'], diagnostic=True)
        source = eligible_source('/System/Volumes/Data', EVIDENCE / 'eligible-source.jsonl',
                                 EVIDENCE / 'eligible-source-native.log')
        free = shutil.disk_usage(WORK).free
        required_bytes = capacity_requirement(source['unique_hardlink_logical_bytes'], source['entries'], CHANGE_BYTES)
        self.save('capacity.json', dict(source=source, free_bytes=free, planning_bytes=required_bytes,
                                       planning_deficit_bytes=max(0, required_bytes - free),
                                       placement='single native local filesystem; no approved remote storage',
                                       uncertainties=['allocated sums can double-count APFS cloned/shared extents',
                                                      'remote compression, snapshot growth and restored sparse allocation not established',
                                                      'live source can change after inventory; not a snapshot size'],
                                       conclusion='planning-headroom-available' if free >= required_bytes else 'local-placement-not-certified; NOT proven platform impossibility or ENOSPC',
                                       private_storage_prerequisite='approved private reachability from hosted Mac; pinned native S3 with measured available storage; independent native full-restore space',
                                       method='two unique-hardlink logical copies + twice changed data + max(8GB,8192 bytes/entry); conservative estimate, not a lower bound'))
        if free < required_bytes:
            raise RuntimeError(f'local placement not certified: measured free {free}B, conservative planning estimate {required_bytes}B. This is NOT proof of impossibility; see capacity.json uncertainty and private-storage prerequisites. No source reduction or infrastructure change authorized.')
        self.event('platform-and-capacity-passed', **source)

    def service(self, argv, name, env=None):
        log = open(EVIDENCE / (name + '.log'), 'xb', buffering=0)
        p = subprocess.Popen([str(x) for x in argv], stdout=log, stderr=subprocess.STDOUT, env=env)
        self.services.append((p, log))
        return p

    def start_services(self):
        import socket
        for port in (445, 19000, 19001, 19002, 19003):
            with socket.socket() as s:
                s.bind(('127.0.0.1', port))
        # Synthetic credentials, confined to task-owned loopback storage.
        os.environ['MINIO_ROOT_USER'] = 'mac-acceptance'
        os.environ['MINIO_ROOT_PASSWORD'] = 'synthetic-mac-acceptance-secret'
        self.service([BIN / 'minio', 'server', '--address', '127.0.0.1:19000',
                      '--console-address', '127.0.0.1:19003', WORK / 'objects'], 'minio')
        deadline = time.monotonic() + 60
        while True:
            if any(p.poll() is not None for p, _ in self.services):
                raise RuntimeError('MinIO exited during startup')
            try:
                with urllib.request.urlopen('http://127.0.0.1:19000/minio/health/ready', timeout=2) as r:
                    if r.status == 200:
                        break
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= deadline:
                    raise RuntimeError('MinIO readiness deadline exceeded')
                time.sleep(.2)
        self.cmd.run([self.fixture, 'bucket-create', '--endpoint', 'http://127.0.0.1:19000', '--bucket', 'time-machine'])
        self.inventory('initial-empty-bucket')
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

    def inventory(self, label):
        output, _ = self.cmd.run([self.fixture, 'bucket-list', '--endpoint', 'http://127.0.0.1:19000', '--bucket', 'time-machine'])
        data = json.loads(output)
        self.save(label + '.json', data)
        if label == 'initial-empty-bucket' and data['object_count'] != 0:
            raise RuntimeError('initial bucket is not empty')
        return data

    def start_daemon(self, phase):
        self.serial += 1
        self.local.mkdir(mode=0o700, exist_ok=True)
        config = self.local / 'config.yaml'
        if not config.exists():
            # These are only documented fresh-install inputs. No old receipt,
            # SQLite, key file or cache is retained across wipe.
            config.write_text(f'''smb:
  listen: 127.0.0.1:445
  share: TimeMachine
  username: timemachine
  password: ""
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
        # Explicit named-empty credentials, no guest or local generic SMB client.
        self.cmd.run(['/sbin/mount_smbfs', '-N', '//timemachine:@127.0.0.1/TimeMachine', self.share])
        self.cmd.run(['/usr/bin/smbutil', 'statshares', '-a'])
        self.cmd.run(['/sbin/mount'])

    def configure_destination(self):
        self.cmd.run(['/usr/bin/tmutil', 'setdestination', 'smb://timemachine:@127.0.0.1/TimeMachine'])
        text, _ = self.cmd.run(['/usr/bin/tmutil', 'destinationinfo', '-X'])
        info = plistlib.loads(text.encode())
        destinations = info['Destinations']
        if len(destinations) != 1:
            raise RuntimeError('expected only task-owned Time Machine destination')
        self.destination = destinations[0]['ID']
        self.save('destination.json', info)

    def source_changes(self, later=False):
        proof = HOME / 's3-smb-acceptance-proof'
        if not later:
            proof.mkdir(mode=0o700)
            (proof / 'original').write_bytes(os.urandom(4_000_000))
            (proof / 'deleted-later').write_text('baseline deletion witness\n')
            os.link(proof / 'original', proof / 'hardlink')
            os.symlink('original', proof / 'symlink')
            os.setxattr(proof / 'original', 'user.s3-smb-proof', b'native metadata')
            os.setxattr(proof / 'original', 'com.apple.ResourceFork', b'native resource fork\n')
            self.cmd.run(['/bin/chmod', '+a', 'everyone allow read', proof / 'original'])
        else:
            (proof / 'deleted-later').unlink()
            with (proof / 'original').open('ab') as f:
                f.write(b'changed after completed baseline\n')
            with (proof / 'later-data').open('xb') as f:
                remaining = CHANGE_BYTES
                while remaining:
                    block = os.urandom(min(4_000_000, remaining))
                    f.write(block)
                    remaining -= len(block)
        text, _ = self.cmd.run(['/usr/bin/tmutil', 'isexcluded', proof])
        if '[Included]' not in text:
            raise RuntimeError('supplemental change directory excluded from normal backup')
        manifest(proof, EVIDENCE / ('later-proof-manifest.jsonl' if later else 'baseline-proof-manifest.jsonl'))
        self.event('source-changed', later=later, path=str(proof))

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
        while process.poll() is None:
            self.daemon.pump()
            if time.monotonic() >= deadline:
                raise RuntimeError('full Time Machine backup exceeded 90 minute stage budget')
            time.sleep(1)
        log.close()
        self.backup = None
        if process.returncode or self.status():
            raise RuntimeError(f'Time Machine did not complete cleanly: {process.returncode}')
        completed = utc()
        self.event('time-machine-command-completed', label=label, completed=completed)
        return completed

    def metadata_point(self, after, label):
        threshold = datetime.datetime.fromisoformat(after)
        deadline = time.monotonic() + 3900  # Native DEFAULT hourly schedule, unchanged.
        receipt_path = self.local / 'state/backup-receipt.json'
        while time.monotonic() < deadline:
            self.daemon.pump()
            if receipt_path.exists():
                receipt = json.loads(receipt_path.read_text())
                snapshot = datetime.datetime.fromisoformat(receipt['Snapshot'].replace('Z', '+00:00'))
                if snapshot > threshold:
                    self.save(label + '-receipt.json', receipt)
                    self.inventory(label + '-objects')
                    self.event('native-point-after-completion', label=label, receipt=receipt)
                    return receipt
            time.sleep(1)
        raise RuntimeError('no successful native metadata point after Time Machine completion')

    def detach_clients(self):
        # Never delete/modify a Time Machine image to make recovery succeed.
        info, _ = self.cmd.run(['/usr/bin/hdiutil', 'info', '-plist'])
        devices_to_detach = list(self.attachments)
        for image in plistlib.loads(info.encode()).get('images', []):
            path = image.get('image-path', '')
            owned_image = path.startswith(str(self.share) + '/') or '/127.0.0.1/' in path
            if path.endswith('.sparsebundle') and owned_image:
                devices = [e['dev-entry'] for e in image.get('system-entities', []) if 'dev-entry' in e]
                if devices and devices[0] not in devices_to_detach:
                    devices_to_detach.append(devices[0])
        for device in reversed(devices_to_detach):
            self.cmd.run(['/usr/bin/hdiutil', 'detach', device], timeout=120)
        self.attachments.clear()
        mounts, _ = self.cmd.run(['/sbin/mount'])
        # Include Apple's private network destination mount as well as ours.
        for line in mounts.splitlines():
            if '(smbfs' in line and '127.0.0.1/TimeMachine on ' in line:
                mountpoint = line.split(' on ', 1)[1].split(' (', 1)[0]
                self.cmd.run(['/sbin/umount', mountpoint], timeout=120)
        remaining, _ = self.cmd.run(['/sbin/mount'])
        if any('(smbfs' in line and '127.0.0.1/TimeMachine' in line for line in remaining.splitlines()):
            raise RuntimeError('task SMB mount remains; refusing cold recovery')

    def wipe(self, label):
        if self.daemon and not self.daemon.reaped:
            raise RuntimeError('cannot wipe while application is alive')
        if self.local != WORK / 'daemon' or self.local.is_symlink():
            raise RuntimeError('unexpected daemon-local path')
        self.save(label + '-wiped-paths.json', [str(p.relative_to(self.local)) for p in sorted(self.local.rglob('*'))])
        shutil.rmtree(self.local)
        if self.local.exists():
            raise RuntimeError('daemon-local wipe incomplete')
        self.event('all-daemon-local-data-wiped', label=label)

    def cold_recover(self, label, abrupt=False):
        self.detach_clients()
        if not abrupt:
            # Ordinary intact restart validates receipt through native remote
            # readback before we discard ALL local authority/config/cache/key.
            self.daemon.stop()
            receipt_path = self.local / 'state/backup-receipt.json'
            receipt = json.loads(receipt_path.read_text())
            event_offset = (EVIDENCE / 's3-events.jsonl').stat().st_size
            self.start_daemon('restart')
            if json.loads(receipt_path.read_text()) != receipt:
                raise RuntimeError('ordinary restart did not reuse the protected receipt; native readback not established')
            with (EVIDENCE / 's3-events.jsonl').open() as events:
                events.seek(event_offset)
                readbacks = [json.loads(line) for line in events]
            matching = [e for e in readbacks if e.get('event') == 'response_complete' and
                        e.get('method') == 'GET' and e.get('key', '').endswith(receipt['Key']) and
                        200 <= e.get('status', 0) < 300 and e.get('bytes', 0) > 0]
            self.save(label + '-native-receipt-readback.json', dict(receipt=receipt, remote_reads=matching))
            if not matching:
                raise RuntimeError('no successful remote native receipt readback on ordinary restart')
            self.daemon.stop()
        self.wipe(label)
        self.start_daemon('recover')
        self.mount_share()
        self.inventory(label + '-recovered-objects')

    def remote_backup(self, label, identifier=None):
        # Every restore uses a fresh network sparsebundle attachment, not the
        # source's APFS snapshots nor an earlier locally attached destination.
        bundles = sorted(self.share.glob('*.sparsebundle'))
        if len(bundles) != 1:
            raise RuntimeError(f'expected one real Time Machine sparsebundle, got {bundles}')
        before = self.control()
        text, _ = self.cmd.run(['/usr/bin/hdiutil', 'attach', '-readonly', '-nobrowse', '-plist', bundles[0]], timeout=300)
        entities = plistlib.loads(text.encode())['system-entities']
        devices = [e['dev-entry'] for e in entities if 'dev-entry' in e]
        self.attachments.append(devices[0])
        volumes = [Path(e['mount-point']) for e in entities if 'mount-point' in e]
        if len(volumes) != 1:
            raise RuntimeError('unknown Time Machine image volume layout; refusing partial restore')
        volume = volumes[0]
        output, _ = self.cmd.run(['/usr/bin/tmutil', 'listbackups', '-d', volume], timeout=120)
        backups = [Path(line) for line in output.splitlines() if line.startswith('/')]
        if not backups:
            raise RuntimeError('remote destination has no completed Time Machine backups')
        latest, _ = self.cmd.run(['/usr/bin/tmutil', 'latestbackup', '-d', volume], timeout=120)
        if identifier is None:
            selected = Path(latest.strip())
            if selected not in backups:
                raise RuntimeError('latest backup absent from completed remote backup list')
        else:
            matches = [p for p in backups if p.name == identifier]
            if len(matches) != 1:
                raise RuntimeError('completed baseline not present exactly once after recovery')
            selected = matches[0]
        # APFS Time Machine may mount a remote snapshot at /Volumes/.timemachine;
        # retain hdiutil+diskutil/mount chain for device-to-network-image proof.
        selected_info, _ = self.cmd.run(['/usr/sbin/diskutil', 'info', '-plist', selected])
        image_info, _ = self.cmd.run(['/usr/sbin/diskutil', 'info', '-plist', volume])
        source_info, _ = self.cmd.run(['/usr/sbin/diskutil', 'info', '-plist', '/System/Volumes/Data'])
        selected_disk, image_disk, source_disk = [plistlib.loads(x.encode()) for x in (selected_info, image_info, source_info)]
        parent = image_disk.get('ParentWholeDisk')
        if not parent or selected_disk.get('ParentWholeDisk') != parent or source_disk.get('ParentWholeDisk') == parent:
            raise RuntimeError('selected backup is not proven to belong to the remote attached image device')
        if not selected_disk.get('ReadOnlyVolume'):
            raise RuntimeError('selected backup is not read-only; cannot manifest stable native backup')
        self.cmd.run(['/usr/bin/hdiutil', 'info', '-plist'])
        self.cmd.run(['/sbin/mount'])
        self.save(label + '-remote-selection.json', dict(image=str(bundles[0]), device=devices[0],
                  image_volume=str(volume), completed_backups=list(map(str, backups)), selected=str(selected)))
        if not selected.is_dir() or selected.name.endswith('.inProgress'):
            raise RuntimeError('not a completed remote backup directory')
        return selected, before

    def completed_before_wipe(self, label):
        selected, _ = self.remote_backup(label + '-before-wipe')
        manifest(selected, EVIDENCE / (label + '-before-wipe-manifest.jsonl'))
        if label == 'baseline':
            self.verify_source_coverage(selected)
        identifier = selected.name
        self.detach_clients()
        self.mount_share()
        return identifier

    def data_root(self, selected):
        candidates = [p for p in selected.iterdir() if p.is_dir() and
                      p.name in (self.source_volume_name, 'Data', 'Macintosh HD - Data')]
        if len(candidates) != 1:
            raise RuntimeError('cannot identify full backed-up source Data volume; refusing fixture-only coverage')
        return candidates[0]

    def verify_source_coverage(self, selected):
        root = self.data_root(selected)
        missing = 0
        checked = 0
        with (EVIDENCE / 'eligible-source.jsonl').open() as source, (EVIDENCE / 'baseline-source-coverage.jsonl').open('x') as out:
            for line in source:
                row = json.loads(line)
                if not row['included']:
                    continue
                relative = Path(row['path']).relative_to('/System/Volumes/Data')
                target = root / relative
                exists = os.path.lexists(target)
                out.write(json.dumps(dict(source=row['path'], backup=str(target), present=exists)) + '\n')
                checked += 1
                missing += not exists
        self.save('source-coverage-summary.json', dict(checked=checked, missing=missing,
                  note='preflight normally eligible namespace vs completed backup; live-source removals are not silently waived'))
        if missing:
            raise RuntimeError(f'{missing} eligible source entries absent from completed backup; inspect coverage evidence')

    def restore_full(self, label, identifier, compare_to):
        selected, before = self.remote_backup(label, identifier)
        restore = WORK / 'restore'
        restore.mkdir(mode=0o700)  # Must be absent/empty each time.
        expected = EVIDENCE / (label + '-backup-manifest.jsonl')
        actual = EVIDENCE / (label + '-restored-manifest.jsonl')
        # Whole completed backup namespace; no proof-file filter. tmutil restore
        # operates on each complete backed-up volume, not a custom image reader.
        roots = sorted(selected.iterdir(), key=lambda p: os.fsencode(p.name))
        if not roots or any(not p.is_dir() or p.is_symlink() for p in roots):
            raise RuntimeError('unrecognized backup volume-root layout; cannot assert complete native restore')
        counts = manifest(selected, expected)
        if not counts['files']:
            raise RuntimeError('empty backup manifest cannot establish full source coverage')
        compare(EVIDENCE / (compare_to + '-manifest.jsonl'), expected,
                EVIDENCE / (label + '-prewipe-differences.jsonl'))
        restore_reads_before = self.control()
        self.event('native-full-restore-start', label=label)
        for root in roots:
            self.cmd.run(['/usr/bin/tmutil', 'restore', '-v', root, restore / root.name], timeout=5400, capture=False)
        restore_reads_after = self.control()
        self.save(label + '-native-restore-reads.json', dict(before=restore_reads_before, after=restore_reads_after))
        if restore_reads_after['chunk_get_success'] <= restore_reads_before['chunk_get_success'] or restore_reads_after['chunk_get_success_bytes'] <= restore_reads_before['chunk_get_success_bytes']:
            raise RuntimeError('tmutil restore itself did not demonstrate remote chunk reads; manifest reads alone are insufficient')
        self.event('native-full-restore-returned', label=label)
        # The container is test-created; compare complete volume roots separately
        # so its invented timestamps do not masquerade as backed-up metadata.
        restored_counts = manifest(restore, actual)
        self.compare_volume_manifests(expected, actual, label)
        proof = self.data_root(restore) / HOME.relative_to('/') / 's3-smb-acceptance-proof'
        proof_actual = EVIDENCE / (label + '-restored-proof.jsonl')
        manifest(proof, proof_actual)
        proof_expected = EVIDENCE / ('later-proof-manifest.jsonl' if label == 'resumed' else 'baseline-proof-manifest.jsonl')
        compare(proof_expected, proof_actual, EVIDENCE / (label + '-proof-differences.jsonl'))
        after = self.control()
        self.save(label + '-remote-reads.json', dict(before=before, after=after))
        if after['chunk_get_success'] <= before['chunk_get_success'] or after['chunk_get_success_bytes'] <= before['chunk_get_success_bytes']:
            raise RuntimeError('full restore did not demonstrate successful remote chunk reads')
        self.save(label + '-manifest-counts.json', dict(backup=counts, restored=restored_counts))
        self.event('complete-native-remote-restore-verified', label=label, identifier=selected.name, **counts)
        self.detach_clients()
        # Only task-owned restored copies; complete evidence remains retained.
        self.cmd.run(['/bin/chflags', '-R', 'nouchg,noschg', restore])
        shutil.rmtree(restore)
        self.mount_share()
        return selected.name

    def compare_volume_manifests(self, expected, actual, label):
        paths = []
        for path in (expected, actual):
            filtered = Path(str(path) + '.volumes')
            with path.open() as src, filtered.open('x') as dst:
                for line in src:
                    if json.loads(line)['path'] != '.':
                        dst.write(line)
            paths.append(filtered)
        compare(*paths, EVIDENCE / (label + '-restore-differences.jsonl'))

    def crash_during_later_backup(self):
        self.source_changes(later=True)
        self.save('hold-armed.json', self.control('hold', 'POST'))
        self.start_backup('interrupted')
        deadline = time.monotonic() + 600
        while time.monotonic() < deadline:
            self.daemon.pump()
            state = self.control()
            hold = state['hold']
            if state['pending_count'] == 1 and hold and hold['pending'] and 200 <= hold['upstream_status'] < 300:
                if not self.status():
                    raise RuntimeError('held write without active Time Machine backup')
                # Confirm hold is still pending after the active-status evidence.
                state = self.control()
                if state['pending_count'] != 1 or not state['hold']['pending']:
                    raise RuntimeError('write response ceased being held before crash')
                self.save('crash-boundary.json', state)
                self.event('kill-application-during-active-time-machine', pid=self.daemon.pid,
                           hold=state['hold'], boundary='upstream committed successful PUT; response pending to application')
                self.daemon.stop(abrupt=True)
                self.save('after-kill.json', self.control())
                self.control('release', 'POST')
                self.cmd.run(['/usr/bin/tmutil', 'stopbackup'], timeout=120)
                process, log = self.backup
                try:
                    process.wait(timeout=120)
                except subprocess.TimeoutExpired:
                    raise RuntimeError('interrupted native client did not stop')
                log.close()
                self.backup = None
                self.event('interrupted-client-stopped', exit=process.returncode)
                if self.status():
                    raise RuntimeError('Time Machine remained active after stopbackup')
                return
            if self.backup[0].poll() is not None:
                raise RuntimeError('later backup ended without observed held S3 write')
            time.sleep(.1)
        raise RuntimeError('no held real Time Machine chunk PUT within ten minutes')

    def run(self):
        self.platform()
        self.start_services()
        self.start_daemon('initialize')
        self.mount_share()
        if list(self.share.iterdir()):
            raise RuntimeError('initial application share not empty')
        self.configure_destination()
        self.source_changes()
        self.start_backup('baseline')
        completed = self.complete_backup('baseline')
        self.baseline = self.completed_before_wipe('baseline')
        self.metadata_point(completed, 'baseline')
        self.cold_recover('normal')
        self.restore_full('normal', self.baseline, compare_to='baseline-before-wipe')
        self.crash_during_later_backup()
        self.cold_recover('crash', abrupt=True)
        self.restore_full('crash', self.baseline, compare_to='baseline-before-wipe')
        self.start_backup('resumed')
        completed = self.complete_backup('resumed')
        resumed = self.completed_before_wipe('resumed')
        self.metadata_point(completed, 'resumed')
        self.cold_recover('resumed')
        final = self.restore_full('resumed', resumed, compare_to='resumed-before-wipe')
        if final == self.baseline:
            raise RuntimeError('resumed backup did not produce a new completed identifier')
        self.inventory('final-objects')
        return dict(baseline=self.baseline, resumed=final,
                    limitation='application SIGKILL only; not VM power loss or S3 storage loss')

    def finish(self):
        errors = []
        outcomes = []

        def attempt(label, action):
            try:
                return action()
            except Exception as e:
                errors.append(f'{label}: {e}')
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
            attempt('application stop', self.daemon.stop)
            outcomes.append(dict(process='application', pid=self.daemon.pid,
                                 reaped=self.daemon.reaped, status=self.daemon.exit_status))
        for process, log in reversed(self.services):
            attempt('service reap', lambda p=process: reap(p, str(p.args[0]), service=True))
            attempt('service log close', log.close)
        # Diagnostics remain bounded observations, never substitute for a gate.
        for argv in (['/usr/bin/tmutil', 'status'], ['/sbin/mount'],
                     ['/usr/bin/hdiutil', 'info', '-plist'], ['/bin/df', '-k'],
                     ['/usr/bin/log', 'show', '--style', 'json', '--last', '6h',
                      '--predicate', 'process == "backupd" OR process == "backupd-helper"']):
            try:
                self.cmd.run(argv, timeout=90, diagnostic=True, capture=False)
            except Exception as e:
                attempt('diagnostic evidence', lambda: self.event('diagnostic-failed', error=str(e)))
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
            signal.alarm(0)
            try:
                self.finish()
            except BaseException:
                failures.append(traceback.format_exc())
        if failures:
            (EVIDENCE / 'failure.txt').write_text('\n'.join(failures))
            self.event('acceptance-failed', failures=len(failures))
            raise RuntimeError('acceptance failed; see failure.txt and cleanup.json')
        self.event('acceptance-passed', **result)


if __name__ == '__main__':
    # Leave time for bounded diagnostics and artifact upload before the hosted
    # job's six-hour limit, including the preceding build/install stages.
    def deadline_reached(signum, frame):
        raise TimeoutError('final acceptance internal deadline reached; remaining stages are NOT passed')
    signal.signal(signal.SIGALRM, deadline_reached)
    signal.alarm(max(1, int(os.environ['MAC_DEADLINE_EPOCH']) - int(time.time())))
    Acceptance().execute()
