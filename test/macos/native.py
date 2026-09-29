#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded native command evidence and PTY consent."""
from contextlib import contextmanager
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


# TCP payload starts with NetBIOS framing (4 bytes), then the SMB header.
# Select only NEGOTIATE requests, never SESSION_SETUP/authentication or file IO.
NEGOTIATE_FILTER = (
    'ip and tcp dst port 445 and '
    '((tcp[((tcp[12] & 0xf0) >> 2) + 4:4] = 0xff534d42 and '
    'tcp[((tcp[12] & 0xf0) >> 2) + 8] = 0x72) or '
    '(tcp[((tcp[12] & 0xf0) >> 2) + 4:4] = 0xfe534d42 and '
    'tcp[((tcp[12] & 0xf0) >> 2) + 16:2] = 0))'
)


@contextmanager
def negotiate_header_evidence(evidence):
    """Bounded diagnostic for the first real mount, without changing its route."""
    evidence = Path(evidence)
    path = evidence / 'smb-negotiate-header.log'
    # Darwin lo0 DLT_NULL(4) + minimum IPv4/TCP(40) leaves at most 36
    # bytes: NetBIOS(4) + SMB1 header(32). No body/auth bytes are retained.
    # TCP/IP options only reduce captured SMB bytes. Retain tcpdump's actual
    # link-type announcement in this same log; never save a full packet trace.
    argv = ['/usr/sbin/tcpdump', '-i', 'lo0', '-y', 'NULL', '-n', '-s', '80', '-c', '4', '-XX', '-l', NEGOTIATE_FILTER]
    with path.open('xb', buffering=0) as log:
        process = subprocess.Popen(argv, stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
        forced = False
        try:
            deadline = time.monotonic() + 10
            while b'listening on lo0' not in path.read_bytes():
                if process.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError('initial SMB header capture did not start; see smb-negotiate-header.log')
                time.sleep(.05)
            yield
        finally:
            if process.poll() is None:
                process.send_signal(signal.SIGINT)
            try:
                code = process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                forced = True
                process.kill()
                code = process.wait(timeout=5)
            (evidence / 'smb-negotiate-header-status.json').write_text(
                json.dumps(dict(argv=argv, exit=code, forced=forced, ended=utc()), indent=2) + '\n')
            if forced or code != 0:
                raise RuntimeError('initial SMB header capture failed or required kill; see capture status')


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
