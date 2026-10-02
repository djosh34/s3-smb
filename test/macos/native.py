#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded native command evidence and PTY consent."""
import datetime
import json
import math
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


def tm_status_numbers(text):
    """Observe only native numeric progress fields; unknown/absent is not a gate."""
    result = {}
    for native, key in (('Percent', 'tm_percent'), ('bytes', 'tm_bytes'), ('totalBytes', 'tm_total_bytes')):
        match = re.search(rf'(?m)^\s*{native}\s*=\s*"?([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)"?\s*;', text)
        if match:
            number = float(match[1])
            if math.isfinite(number) and number >= 0:
                result[key] = number
    return result


class Commands:
    def __init__(self, evidence):
        self.evidence = Path(evidence)
        self.seq = 0

    def run(self, argv, timeout=120, diagnostic=False, capture=True):
        self.seq += 1
        name = f'{self.seq:04d}-{Path(argv[0]).name}'
        start = utc()
        path = self.evidence / (name + '.log')
        print(f'native-command-start {name} {start}', flush=True)
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
        print(f'native-command-exit {name} code={code} {utc()}', flush=True)
        if code and not diagnostic:
            raise RuntimeError(f'native command failed ({code}): {argv}; see {name}.log')
        return output.decode('utf-8', errors='strict'), code


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
