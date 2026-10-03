# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded startup observation. Never adopt/kill an unowned smbd or change policy."""
import errno
import json
import os
import signal
import time


FIELDS = ('pid', 'ppid', 'pgid', 'start_sec', 'start_usec')


def valid_identity(value):
    return (isinstance(value, dict) and value.get('available') is True
            and value.get('expected_binary') is True
            and all(type(value.get(k)) is int and value[k] >= 0 for k in FIELDS)
            and value['pid'] > 0 and value['start_sec'] > 0 and value['start_usec'] < 1000000)


def same_process(expected, actual):
    return (valid_identity(expected) and valid_identity(actual)
            and all(expected[k] == actual[k] for k in ('pid', 'start_sec', 'start_usec')))


def parse_pids(text, code, lsof=False):
    # A nonzero command with diagnostics is unknown, not an absent listener.
    if code == 1 and not text.strip():
        return []
    lines = text.splitlines()
    if code != 0 or len(lines) > 64:
        return None
    if lsof:
        if any(not line.startswith('p') or not line[1:].isdigit() for line in lines):
            return None
        lines = [line[1:] for line in lines]
    if any(not line.isdigit() or int(line) <= 0 for line in lines):
        return None
    return sorted({int(line) for line in lines})


def listener_owner(before, current, launcher_pid, earliest_start_ns):
    """Own session/group was created by Popen(start_new_session=True).

    A daemon which changes its group/session is intentionally not inferred owned.
    It remains an observed unattributed listener, even after launcher exit0.
    """
    if before.get('complete') is not True or current.get('complete') is not True:
        return 'observation-unavailable', None
    listeners = current['listeners_1445']
    if not listeners:
        return 'listener-absent', None
    if len(listeners) != 1:
        return 'listener-ambiguous', None
    pid = listeners[0]
    identity = current['processes'].get(pid)
    if pid in before['processes'] or pid in before['listeners_1445']:
        return 'preexisting-listener', None
    if not valid_identity(identity):
        return 'identity-unavailable', None
    start_ns = identity['start_sec'] * 1000000000 + identity['start_usec'] * 1000
    if start_ns < earliest_start_ns or identity['pgid'] != launcher_pid:
        return 'listener-unattributed', None
    return ('owned-launcher-listener' if pid == launcher_pid else 'owned-group-child-listener'), identity


class StartupProbe:
    def __init__(self, commands, identity_program, output):
        self.cmd, self.program, self.output = commands, identity_program, output
        self.owned = None

    def identity(self, pid):
        text, code = self.cmd.run([self.program, str(pid)], timeout=2, diagnostic=True)
        try:
            value = json.loads(text) if code == 0 else {}
        except ValueError:
            value = {}
        if valid_identity(value):
            return {k: value[k] for k in (*FIELDS, 'available', 'expected_binary')}
        # No arbitrary fields or native error text escape.
        return dict(available=False, gone=value.get('error') == errno.ESRCH)

    def snapshot(self, deadline):
        result = dict(complete=True, processes={}, listeners_1445=None, listeners_445=None)
        for port in (1445, 445):
            text, code = self.cmd.run(['/usr/sbin/lsof', '-nP', '-a', f'-iTCP:{port}',
                                       '-sTCP:LISTEN', '-t'], timeout=2, diagnostic=True)
            result[f'listeners_{port}'] = parse_pids(text, code)
        text, code = self.cmd.run(['/usr/bin/pgrep', '-x', 'smbd'], timeout=2, diagnostic=True)
        pids = parse_pids(text, code)
        if pids is None or any(result[f'listeners_{p}'] is None for p in (1445, 445)):
            result['complete'] = False
            return result
        pids = sorted(set(pids + result['listeners_1445'] + result['listeners_445']))
        if len(pids) > 8:
            result['complete'] = False
            return result
        for pid in pids:
            if time.monotonic() >= deadline:
                result['complete'] = False
                break
            result['processes'][pid] = self.identity(pid)
        return result

    def record(self, **row):
        with self.output.open('a') as f:
            f.write(json.dumps(dict(monotonic_ns=time.monotonic_ns(), **row)) + '\n')

    def wait(self, launcher, before, earliest_start_ns, timeout=15):
        deadline = time.monotonic() + timeout
        verdict = 'observation-unavailable'
        while time.monotonic() < deadline:
            snapshot = self.snapshot(deadline)
            code = launcher.poll()
            verdict, identity = listener_owner(before, snapshot, launcher.pid, earliest_start_ns)
            self.record(kind='post-launch', launcher_pid=launcher.pid, launcher_exit=code,
                        classification=verdict, snapshot=snapshot)
            if identity is not None:
                # Recheck immediately after the listener snapshot; PID alone is insufficient.
                if same_process(identity, self.identity(identity['pid'])):
                    self.owned = identity
                    return verdict
                verdict = 'identity-changed'
            # Launcher exit is recorded but does not alone prove listener absence.
            time.sleep(1)
        return verdict

    def alive(self):
        return self.owned is not None and same_process(self.owned, self.identity(self.owned['pid']))

    def signal_if_same(self, sig, send=os.kill):
        if self.owned is None:
            return 'not-owned'
        current = self.identity(self.owned['pid'])
        if current.get('gone'):
            return 'already-gone'
        if not same_process(self.owned, current):
            return 'identity-unconfirmed'
        try:
            send(self.owned['pid'], sig)
        except ProcessLookupError:
            return 'already-gone'
        return 'signalled'

    def stop(self):
        # No killall, group signalling, launchd adoption, or unverified PID signal.
        outcome = self.signal_if_same(signal.SIGTERM)
        self.record(kind='owned-stop', outcome=outcome)
        if outcome in ('already-gone', 'not-owned'):
            return
        if outcome != 'signalled':
            raise RuntimeError('owned listener identity unconfirmed at cleanup')
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            current = self.identity(self.owned['pid'])
            if current.get('gone'):
                return
            if not same_process(self.owned, current):
                raise RuntimeError('owned listener identity changed at cleanup')
            time.sleep(.5)
        outcome = self.signal_if_same(signal.SIGKILL)
        self.record(kind='owned-kill', outcome=outcome)
        if outcome not in ('signalled', 'already-gone'):
            raise RuntimeError('refused unverified listener kill')
