#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Disposable hosted-Mac Apple smbd control. No s3-smb, MinIO or Time Machine.

All subprocess output/argv/tracebacks and capture bytes stay in private scratch.
Only fixed-producer metadata plus the reviewed publisher allowlist is uploaded.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import sys
import time

from publish_diagnostic import publish
from smb_control_measure import measure

GIB = 2**30
HERE = Path(__file__).resolve().parent


def save(root, name, value):
    (root / name).write_text(json.dumps(value, indent=2) + '\n')


def digest(path):
    result = hashlib.sha256()
    with Path(path).open('rb') as source:
        while block := source.read(2**20):
            result.update(block)
    return result.hexdigest()


class Control:
    def __init__(self, args):
        self.args = args
        self.work, self.public = Path(args.work), Path(args.public)
        # Public and private must be disjoint, fresh directories (no symlinks).
        self.work = self.work.resolve()
        self.public = self.public.resolve()
        if (self.work == self.public or self.work in self.public.parents
                or self.public in self.work.parents):
            raise ValueError('private/public paths overlap')
        if self.work.exists() or self.public.exists():
            raise ValueError('fresh private/public directories required')
        self.work.mkdir(mode=0o711)
        self.work.chmod(0o711)  # SMB account can traverse, not list scratch.
        self.private = self.work / 'private'
        self.private.mkdir(mode=0o700)
        self.share, self.mount = self.work / 'share', self.work / 'mount'
        self.share.mkdir(mode=0o700)
        self.mount.mkdir(mode=0o700)
        self.log = (self.private / 'commands-private.log').open('wb')
        self.health = (self.private / 'capture-health.jsonl').open('w')
        self.state = dict(workload_ok=False, unmount_ok=False, health_ok=True)
        self.capture = None
        self.capture_log = None
        self.mounted = False
        self.stage = 'preflight'
        self.deadline = time.monotonic() + 1200
        self.raw = self.work / 'private-traffic.pcap'

    def command(self, argv, timeout=90, optional=False):
        self.log.write(('stage=' + self.stage + '\n').encode())
        self.log.flush()
        # Never allow CalledProcessError/TimeoutExpired to print credential argv.
        result = subprocess.run(list(map(str, argv)), stdout=self.log, stderr=self.log, timeout=timeout)
        if result.returncode and not optional:
            raise RuntimeError('private command failed')
        return result.returncode

    def checked_space(self):
        return shutil.disk_usage(self.work).free

    def sample(self):
        row = dict(time=time.time(), capture_exit=self.capture.poll(), free_bytes=self.checked_space(),
                   raw_bytes=self.raw.stat().st_size if self.raw.exists() else 0)
        self.health.write(json.dumps(row) + '\n')
        self.health.flush()
        if row['capture_exit'] is not None or row['free_bytes'] < 20 * GIB or row['raw_bytes'] > 60 * GIB:
            self.state['health_ok'] = False
            raise RuntimeError('capture/resource health failed')
        if time.monotonic() > self.deadline:
            raise RuntimeError('control deadline exceeded')

    def monitored(self, argv, output, timeout):
        with output.open('wb') as log:
            process = subprocess.Popen(list(map(str, argv)), stdout=log, stderr=self.log)
            end = time.monotonic() + timeout
            try:
                while process.poll() is None:
                    self.sample()
                    if time.monotonic() > end:
                        raise RuntimeError('workload command timed out')
                    time.sleep(1)
                if process.returncode:
                    raise RuntimeError('workload command failed')
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=10)

    def run(self):
        if self.checked_space() < 75 * GIB:
            raise RuntimeError('75 GiB free required')
        provenance = dict(harness=subprocess.check_output(['git', '-C', HERE, 'rev-parse', 'HEAD'], text=True).strip(),
            dirty=bool(subprocess.check_output(['git', '-C', HERE, 'status', '--porcelain'])),
            macos=subprocess.check_output(['/usr/bin/sw_vers'], text=True).splitlines(),
            python=sys.version.split()[0], scripts={p.name: digest(p) for p in HERE.glob('*control*.py')},
            framing_capture_sha256=digest(HERE / 'framing_capture.py'),
            publisher_sha256=digest(HERE / 'publish_diagnostic.py'),
            smbd_sha256=digest('/usr/sbin/smbd'),
            smbfs_binary_sha256=None, transport='IPv4 loopback port 445; no PF/proxy',
            fixture=dict(mib=self.args.mib, seconds=self.args.seconds, workers=4, slot_mib=1024,
                         application_write_bytes=2**20))
        kext = Path('/System/Library/Extensions/smbfs.kext/Contents/MacOS/smbfs')
        if kext.exists():
            provenance['smbfs_binary_sha256'] = digest(kext)
        save(self.private, 'control-provenance.json', provenance)
        password = secrets.token_hex(16)
        self.stage = 'apple-server-setup'
        self.command(['/usr/sbin/sysadminctl', '-addUser', 'swarm64', '-password', password])
        self.command(['/usr/bin/pwpolicy', '-u', 'swarm64', '-sethashtypes', 'SMB-NT', 'on'], optional=True)
        self.command(['/usr/bin/dscl', '.', '-passwd', '/Users/swarm64', password])
        self.command(['/usr/sbin/chown', 'swarm64', self.share])
        self.command(['/usr/sbin/sharing', '-a', self.share, '-n', 'Swarm64', '-S', 'Swarm64', '-s', '001', '-g', '000'])
        self.command(['/bin/launchctl', 'enable', 'system/com.apple.smbd'])
        self.command(['/bin/launchctl', 'bootstrap', 'system', '/System/Library/LaunchDaemons/com.apple.smbd.plist'], optional=True)
        self.command(['/bin/launchctl', 'kickstart', '-k', 'system/com.apple.smbd'], optional=True)
        self.stage = 'capture-start'
        self.capture_log = (self.private / 'tcpdump-stderr.log').open('wb')
        self.capture = subprocess.Popen(['/usr/sbin/tcpdump', '-i', 'lo0', '-nn', '-s', '0', '-B', '131072',
            '-U', '-w', str(self.raw), 'ip and tcp and host 127.0.0.1 and port 445'],
            stdout=subprocess.DEVNULL, stderr=self.capture_log)
        end = time.monotonic() + 15
        while b'listening on lo0' not in (self.private / 'tcpdump-stderr.log').read_bytes():
            self.sample()
            if time.monotonic() > end:
                raise RuntimeError('capture not ready')
            time.sleep(.1)
        self.stage = 'mount'
        self.command(['/sbin/mount_smbfs', '-N', f'//swarm64:{password}@127.0.0.1/Swarm64', self.mount], timeout=60)
        self.mounted = True
        del password
        # statshares stays private: no arbitrary command output gets published.
        self.command(['/usr/bin/smbutil', 'statshares', '-a'])
        self.stage = 'writes'
        self.state['write_start'] = time.time()
        self.monitored([sys.executable, HERE / 'smb_control_write.py', self.mount / 'load',
            '--mib', self.args.mib, '--seconds', self.args.seconds], self.private / 'write.jsonl', timeout=900)
        self.state['write_end'] = time.time()
        self.stage = 'server-local-hash-check'
        self.monitored([sys.executable, HERE / 'smb_control.py', '--verify', self.private / 'write.jsonl',
            self.share / 'load'], self.private / 'server-file-check.json', timeout=180)
        self.state['workload_ok'] = True

    def finish(self):
        self.state.setdefault('write_end', time.time())
        self.state['last_stage'] = self.stage
        self.stage = 'unmount'
        try:
            # Also attempt after a mount timeout, which can leave a live mount.
            code = self.command(['/sbin/umount', self.mount], timeout=60, optional=True)
            self.state['unmount_ok'] = self.mounted and code == 0
        except Exception:
            self.state['unmount_ok'] = False
        if self.capture:
            try:
                self.sample()
            except Exception:
                self.state['health_ok'] = False
            self.state['capture_alive_before_stop'] = self.capture.poll() is None
            if self.capture.poll() is None:
                self.capture.send_signal(signal.SIGINT)
            try:
                self.state['tcpdump_exit'] = self.capture.wait(timeout=30)
            except subprocess.TimeoutExpired:
                self.capture.kill()
                self.state['tcpdump_exit'] = self.capture.wait(timeout=10)
            self.capture_log.close()
        self.health.close()
        self.stage = 'offline-parse'
        try:
            self.state['parser_exit'] = self.command([sys.executable, HERE / 'framing_capture.py',
                self.raw, self.private / 'framing'], timeout=300, optional=True)
        except Exception:
            self.state['parser_exit'] = -1
        verdict = measure(self.private, self.state, self.args.mib * 2**20,
                          self.args.seconds - 5, 2**20)
        save(self.private, 'measurement.json', verdict)
        save(self.private, 'control-state.json', self.state)
        self.log.close()
        # Reviewed allowlist strips raw header4. No private-directory upload fallback.
        publish(self.private, self.public)
        for name in ('control-provenance.json', 'control-state.json', 'write.jsonl', 'server-file-check.json'):
            source = self.private / name
            if source.exists():
                shutil.copyfile(source, self.public / name)
        return verdict['clean_control']


def verify(log, root):
    with Path(log).open() as source:
        result = next(json.loads(line) for line in source if '"event": "control-write-complete"' in line)
    for file in result['files']:
        path = Path(root) / file['file']
        if path.stat().st_size != file['size'] or digest(path) != file['sha256']:
            raise RuntimeError('server-local content mismatch')
    print(json.dumps(dict(server_files_verified=len(result['files']))))


def main():
    if len(sys.argv) == 4 and sys.argv[1] == '--verify':
        verify(*sys.argv[2:])
        return 0
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--work', required=True)
    parser.add_argument('--public', required=True)
    parser.add_argument('--mib', type=int, default=24576)
    parser.add_argument('--seconds', type=int, default=384)
    args = parser.parse_args()
    if sys.platform != 'darwin' or os.geteuid() != 0:
        parser.error('requires root on a disposable allocated hosted Mac')
    if args.mib != 24576 or args.seconds != 384:
        parser.error('initial control profile is fixed at 24 GiB / 384 seconds')
    os.umask(0o077)
    control = Control(args)
    try:
        control.run()
    except Exception:
        # No arbitrary error strings: mount argv may contain fixture credentials.
        control.state['failed'] = True
    return 0 if control.finish() else 1


if __name__ == '__main__':
    # Suppress exception text even when publication/cleanup fails.
    try:
        code = main()
    except Exception:
        print('Apple control failed; private diagnostics withheld.', file=sys.stderr)
        code = 1
    sys.exit(code)
