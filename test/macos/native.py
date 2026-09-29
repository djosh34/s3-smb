#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded native command evidence, eligibility measurement and PTY consent."""
import datetime
import json
import os
from pathlib import Path
import pty
import re
import signal
import subprocess
import time
import threading


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def verify_tmutil_verb(manual, verb, required_options, help_code):
    """Brief help is evidence, not an exhaustive capability/option listing.

    The delivered manual supplies each verb's contract. Real operations remain
    the acceptance gate; documentation alone never proves SMB compatibility.
    """
    if help_code not in (0, 1):
        raise RuntimeError(f'cannot collect installed tmutil help for {verb}: {help_code}')
    section = re.search(rf'(?ms)^ {{5}}{re.escape(verb)}\b.*?(?=^ {{5}}[a-z][a-z0-9]*\b|^[A-Z][A-Z ]*$|\Z)', manual)
    if not section:
        raise RuntimeError(f'installed tmutil manual does not document {verb}')
    text = section.group()
    # Native usage often groups short options, e.g. [-pv]. Do not require a
    # particular synopsis spelling or infer absence from terse per-verb help.
    options = set(re.findall(r'(?<![\w-])--[a-z][a-z-]*', text))
    for group in re.findall(r'(?<![\w-])-(?!-)([A-Za-z]+)\b', text):
        options.update('-' + letter for letter in group)
    if not set(required_options) <= options:
        raise RuntimeError(f'installed tmutil manual does not establish {verb} {required_options}')
    if verb == 'setdestination' and not (
            re.search(r'\bSMB\b', text, re.IGNORECASE) and
            re.search(r'(?:protocol|smb)://user\[:pass\]@host/share', text, re.IGNORECASE)):
        raise RuntimeError('installed tmutil manual does not establish the SMB destination URL form')


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
            argv = ['/usr/bin/tmutil', 'isexcluded', *map(str, batch)]
            raw.write(('# argv: ' + json.dumps(argv) + '\n').encode())
            r = subprocess.run(argv, capture_output=True, timeout=120)
            raw.write(r.stdout + r.stderr)
            if r.returncode:
                raise RuntimeError('tmutil isexcluded failed; see full eligibility log')
            lines = r.stdout.decode().splitlines()
            if len(lines) != len(batch):
                raise RuntimeError('unexpected tmutil isexcluded output count')
            for path, line in zip(batch, lines):
                m = re.fullmatch(r'\[(Included|Excluded)\]\s+(.*)', line)
                matches = m is not None and m[2] == str(path)
                if m and not matches:
                    # Accept alternate spellings only when the OS confirms identity.
                    try:
                        matches = path.samefile(m[2])
                    except OSError:
                        pass
                if not matches:
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
        self.exit_status = None
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
            self.exit_status = status
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

    def stop(self, abrupt=False, timeout=45):
        if self.reaped:
            return
        os.kill(self.pid, signal.SIGKILL if abrupt else signal.SIGTERM)
        deadline = time.monotonic() + timeout
        forced = False
        while True:
            pid, status = os.waitpid(self.pid, os.WNOHANG)
            if pid:
                self.reaped = True
                self.exit_status = status
                self.close_evidence()
                expected = os.WIFSIGNALED(status) and os.WTERMSIG(status) == signal.SIGKILL if abrupt else os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0
                if forced or not expected:
                    raise RuntimeError(f'application shutdown failed (forced={forced}, status={status})')
                return
            if time.monotonic() >= deadline:
                if forced:
                    raise RuntimeError('application could not be reaped within forced-kill deadline')
                os.kill(self.pid, signal.SIGKILL)
                forced = True
                deadline = time.monotonic() + 5
            time.sleep(.1)
