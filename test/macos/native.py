#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded native command evidence, eligibility measurement and PTY consent."""
import datetime
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import subprocess
import time
import threading


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


class Commands:
    def __init__(self, evidence):
        self.evidence = Path(evidence)
        self.seq = 0

    def run(self, argv, timeout=120, diagnostic=False, capture=True):
        self.seq += 1
        name = f'{self.seq:04d}-{Path(argv[0]).name}'
        start = utc()
        path = self.evidence / (name + '.log')
        with path.open('xb') as log:
            try:
                result = subprocess.run([str(x) for x in argv], stdout=log,
                                        stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL, timeout=timeout)
                code = result.returncode
            except subprocess.TimeoutExpired:
                code = 124
        output = path.read_bytes() if capture else b''
        with (self.evidence / 'commands.jsonl').open('a') as f:
            f.write(json.dumps(dict(argv=[str(x) for x in argv], start=start, end=utc(),
                                   exit=code, diagnostic=diagnostic, output=name + '.log')) + '\n')
        if code and not diagnostic:
            raise RuntimeError(f'native command failed ({code}): {argv}; see {name}.log')
        return output.decode('utf-8', errors='strict'), code


def eligible_source(root, output, raw_output):
    """Ask installed tmutil for every encountered entry; prune excluded trees only."""
    counts = dict(entries=0, files=0, logical_bytes=0, allocated_bytes=0,
                  unique_hardlink_logical_bytes=0, unique_hardlink_allocated_bytes=0, excluded=0)
    seen = set()
    root = Path(root)
    dev = root.stat().st_dev
    with open(output, 'x') as out, open(raw_output, 'xb') as raw:
        pending = [root]
        while pending:
            batch, pending = pending[:100], pending[100:]
            if any('\n' in str(p) or '\r' in str(p) for p in batch):
                raise RuntimeError('tmutil text eligibility cannot unambiguously represent newline path')
            r = subprocess.run(['/usr/bin/tmutil', 'isexcluded', *map(str, batch)],
                               capture_output=True, timeout=120)
            raw.write(r.stdout + r.stderr)
            if r.returncode:
                raise RuntimeError('tmutil isexcluded failed; see full eligibility log')
            lines = r.stdout.decode().splitlines()
            if len(lines) != len(batch):
                raise RuntimeError('unexpected tmutil isexcluded output count')
            for path, line in zip(batch, lines):
                m = re.fullmatch(r'\[(Included|Excluded)\]\s+(.*)', line)
                if not m or m[2] != str(path):
                    raise RuntimeError(f'unrecognized native eligibility result: {line!r}')
                included = m[1] == 'Included'
                row = {'path': str(path), 'included': included}
                if included:
                    s = path.lstat()
                    row.update(mode=s.st_mode, size=s.st_size, blocks=s.st_blocks, device=s.st_dev)
                    if s.st_dev != dev:
                        raise RuntimeError(f'additional eligible mounted filesystem needs explicit coverage: {path}')
                    counts['entries'] += 1
                    import stat
                    if stat.S_ISREG(s.st_mode):
                        counts['files'] += 1
                        counts['logical_bytes'] += s.st_size
                        counts['allocated_bytes'] += s.st_blocks * 512
                        identity = (s.st_dev, s.st_ino)
                        if identity not in seen:
                            seen.add(identity)
                            counts['unique_hardlink_logical_bytes'] += s.st_size
                            counts['unique_hardlink_allocated_bytes'] += s.st_blocks * 512
                    if stat.S_ISDIR(s.st_mode):
                        pending.extend(sorted(path.iterdir(), key=lambda p: os.fsencode(p.name)))
                else:
                    counts['excluded'] += 1
                out.write(json.dumps(row, sort_keys=True) + '\n')
    if not counts['files'] or not counts['logical_bytes']:
        raise RuntimeError('empty normally eligible source is not full-Mac acceptance')
    return counts


def capacity_requirement(source_bytes, entries, change_bytes):
    # No assumed compression/deduplication/clones. Remote baseline + changed
    # chunks + one sequential full restore, and metadata/working-space reserve.
    return 2 * source_bytes + 2 * change_bytes + max(8_000_000_000, entries * 8192)


class Daemon:
    def __init__(self, executable, config, log, phase):
        self.pid, fd = pty.fork()
        if self.pid == 0:
            os.execv(str(executable), [str(executable), '-c', str(config), 'serve'])
        self.fd = fd
        self.log = open(log, 'xb', buffering=0)
        self.buffer = b''
        self.phase = phase
        self.confirmed = False
        self.reaped = False
        self.reader_error = None
        self.ready_seen = False
        self.reader = threading.Thread(target=self.drain, daemon=True)
        self.reader.start()

    def drain(self):
        try:
            while True:
                try:
                    data = os.read(self.fd, 65536)
                except OSError as e:
                    # Linux PTY close yields EIO; Darwin generally returns EOF.
                    if e.errno == 5:
                        break
                    raise
                if not data:
                    break
                self.log.write(data)
                self.buffer = (self.buffer + data)[-131072:]
                self.ready_seen = self.ready_seen or b'"msg":"SMB serving"' in self.buffer
                if b'Continue? [yes/no]: ' in self.buffer and not self.confirmed:
                    expected = b'Initialize a genuinely empty S3 dataset?' if self.phase == 'initialize' else b'Recover metadata from '
                    if self.phase == 'restart' or expected not in self.buffer:
                        raise RuntimeError('unexpected application confirmation; refusing automatic answer')
                    os.write(self.fd, b'yes\n')
                    self.confirmed = True
        except BaseException as e:
            self.reader_error = e

    def pump(self):
        if self.reader_error:
            raise self.reader_error
        pid, status = os.waitpid(self.pid, os.WNOHANG)
        if pid:
            self.reaped = True
            self.close_evidence()
            raise RuntimeError(f'application exited unexpectedly: {status}')

    def ready(self):
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            self.pump()
            if self.ready_seen:
                if self.phase != 'restart' and not self.confirmed:
                    raise RuntimeError('fresh start did not require documented confirmation')
                return
            time.sleep(.1)
        raise RuntimeError('application startup timeout')

    def close_evidence(self):
        self.reader.join(timeout=5)
        if self.reader.is_alive():
            raise RuntimeError('PTY evidence reader did not close')
        os.close(self.fd)
        self.log.close()
        if self.reader_error:
            raise self.reader_error

    def stop(self, abrupt=False):
        if self.reaped:
            return
        os.kill(self.pid, signal.SIGKILL if abrupt else signal.SIGTERM)
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            time.sleep(.1)
            pid, status = os.waitpid(self.pid, os.WNOHANG)
            if pid:
                self.reaped = True
                self.close_evidence()
                expected = os.WIFSIGNALED(status) and os.WTERMSIG(status) == signal.SIGKILL if abrupt else os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0
                if not expected:
                    raise RuntimeError(f'unexpected application stop status: {status}')
                return
        raise RuntimeError('application did not stop within 45s; state must not be wiped')
