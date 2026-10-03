#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Low-rate numeric host observations; never inspect command lines or payloads."""
import json
import math
import os
import re
import shutil
import subprocess
import threading
import time

ROLES = ('server', 'object_store', 'capture', 'backup_client', 'sampler')
INTERVAL_NS = 5_000_000_000


def parse_ps(text, expected):
    """Parse only requested PIDs and four allowlisted columns; fail closed."""
    if len(text) > 4096:
        raise ValueError('oversized process observation')
    result = {}
    for line in text.splitlines():
        columns = line.split()
        if len(columns) != 4:
            raise ValueError('invalid numeric process observation')
        pid, cpu, rss, elapsed = columns
        if not pid.isdecimal() or not rss.isdecimal() or not re.fullmatch(r'\d+(?:\.\d+)?', cpu):
            raise ValueError('invalid numeric process observation')
        pid, cpu, rss = int(pid), float(cpu), int(rss)
        match = re.fullmatch(r'(?:(\d+)-)?(?:(\d+):)?(\d+):(\d+(?:\.\d+)?)', elapsed)
        if not match or pid not in expected or pid in result or not math.isfinite(cpu):
            raise ValueError('invalid numeric process observation')
        days, hours, minutes, seconds = match.groups()
        seconds = float(seconds)
        if seconds >= 60 or (hours is not None and int(minutes) >= 60) or not math.isfinite(seconds):
            raise ValueError('invalid process CPU time')
        total = int(days or 0) * 86400 + int(hours or 0) * 3600 + int(minutes) * 60 + seconds
        if not math.isfinite(total):
            raise ValueError('invalid process CPU time')
        result[pid] = dict(cpu_percent=cpu, rss_kib=rss, cpu_total_seconds=total)
    return result


class NumericResourceSampler:
    """One private JSONL file, known PIDs only, no internal application observer.

    Call set_pids when the harness starts/stops a process (None clears a role).
    PID reuse is not detected: the harness owns process identity/lifetime.
    stop() returns numeric health; callers must retain it even if output fails.
    """
    def __init__(self, output, workpath):
        self.output, self.workpath = output, workpath
        self._pids = {role: None for role in ROLES}
        self._pids['sampler'] = os.getpid()
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread = None
        self._file = None
        self._samples = self._missed = self._errors = self._write_errors = 0
        self._origin = None

    def set_pids(self, **pids):
        if any(role not in ROLES or (pid is not None and (type(pid) is not int or pid <= 0))
               for role, pid in pids.items()):
            raise ValueError('unknown role or invalid PID')
        with self._lock:
            self._pids.update(pids)

    def summary(self):
        return dict(samples=self._samples, missed_intervals=self._missed,
                    observation_errors=self._errors, write_errors=self._write_errors)

    def _observe(self, scheduled):
        start = time.monotonic_ns()
        with self._lock:
            pids = dict(self._pids)
        wanted = {pid for pid in pids.values() if pid is not None}
        rows, ps_status = {}, 'ok'
        if wanted:
            try:
                result = subprocess.run(
                    ['/bin/ps', '-p', ','.join(str(pid) for pid in sorted(wanted)),
                     '-o', 'pid=,pcpu=,rss=,time='],
                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                    stdin=subprocess.DEVNULL, timeout=1, check=False,
                    env={**os.environ, 'LC_ALL': 'C'})
                # ps returns 1 when all requested processes have exited.
                if result.returncode not in (0, 1):
                    ps_status = 'unavailable'
                else:
                    rows = parse_ps(result.stdout.decode('ascii'), wanted)
                    if result.returncode == 1 and rows:
                        rows, ps_status = {}, 'unavailable'
            except subprocess.TimeoutExpired:
                ps_status = 'timeout'
            except (OSError, ValueError, OverflowError):
                ps_status = 'unavailable'
        processes = {}
        for role, pid in pids.items():
            status = 'unconfigured' if pid is None else ('ok' if pid in rows else
                     ('missing' if ps_status == 'ok' else 'unavailable'))
            processes[role] = dict(pid=pid, status=status, cpu_percent=None,
                                  rss_kib=None, cpu_total_seconds=None,
                                  threads=None, read_bytes=None, write_bytes=None,
                                  threads_io_status='unavailable')
            processes[role].update(rows.get(pid, {}))
        free, disk_status = None, 'ok'
        try:
            free = shutil.disk_usage(self.workpath).free
        except OSError:
            disk_status = 'unavailable'
        load, load_status = None, 'ok'
        try:
            load = list(os.getloadavg())
            if len(load) != 3 or any(not math.isfinite(x) or x < 0 for x in load):
                load, load_status = None, 'unavailable'
        except (OSError, AttributeError):
            load_status = 'unavailable'
        self._errors += sum(status != 'ok' for status in (ps_status, disk_status, load_status))
        end = time.monotonic_ns()
        return dict(schema_version=1, kind='sample', sequence=self._samples,
                    interval_ns=INTERVAL_NS, scheduled_monotonic_ns=scheduled,
                    start_monotonic_ns=start, end_monotonic_ns=end,
                    elapsed_ns=start - self._origin, lag_ns=max(0, start - scheduled),
                    duration_ns=end - start, processes=processes, ps_status=ps_status,
                    disk_free_bytes=free, disk_status=disk_status, load_1_5_15=load,
                    load_status=load_status, **self.summary())

    def _emit(self, row):
        try:
            self._file.write(json.dumps(row, separators=(',', ':'), allow_nan=False) + '\n')
            self._file.flush()
        except (OSError, ValueError):
            self._write_errors += 1

    def _run(self):
        scheduled = self._origin
        try:
            while not self._stop.is_set():
                self._emit(self._observe(scheduled))
                self._samples += 1
                scheduled += INTERVAL_NS
                now = time.monotonic_ns()
                if now > scheduled:
                    missed = (now - scheduled) // INTERVAL_NS + 1
                    self._missed += missed
                    scheduled += missed * INTERVAL_NS
                self._stop.wait(max(0, scheduled - now) / 1e9)
        except Exception:
            # Preserve only a counter; exception text can contain private paths.
            self._errors += 1
        finally:
            self._emit(dict(schema_version=1, kind='end', end_monotonic_ns=time.monotonic_ns(),
                            **self.summary()))

    def start(self):
        if self._thread is not None:
            raise RuntimeError('resource sampler already started')
        fd = os.open(self.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        self._file = os.fdopen(fd, 'w', encoding='ascii')
        self._origin = time.monotonic_ns()
        self._thread = threading.Thread(target=self._run, name='numeric-resources', daemon=True)
        self._thread.start()
        return self

    def stop(self):
        if self._thread is None:
            return self.summary()
        self._stop.set()
        self._thread.join(timeout=3)
        if self._thread.is_alive():
            raise RuntimeError('resource sampler did not stop')
        if not self._file.closed:
            try:
                self._file.close()
            except OSError:
                self._write_errors += 1
        return self.summary()
