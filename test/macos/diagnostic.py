#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Ordinary public rc.7 Time Machine backup; capture only, no live/offline parser."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

from client_outcome import collect, status_observations
from acceptance import Acceptance, WORK, EVIDENCE, BIN
from native import utc


class Diagnostic(Acceptance):
    def __init__(self):
        super().__init__()
        self.capture = None
        self.capture_log = None
        self.capture_ready = None
        self.attempt_start_wall = None
        self.attempt_end_wall = None
        self.outcome = dict(attempt=1, parent_attempt=None, workload='time-machine', lifecycle='fresh',
                            start_monotonic_ns=None, end_monotonic_ns=None,
                            run_stage_completed=False, tm_command_completed=False,
                            tm_command_exit=None, completed_backup_selected=False,
                            selection_writable_replay_attempted=False,
                            tree_directory_present=False, restore_performed=False,
                            content_hash_verified=False, stage='not_started')

    def event(self, event, **fields):
        # Native status/paths remain in private evidence, never the Actions log.
        with (EVIDENCE / 'acceptance.jsonl').open('a') as f:
            f.write(json.dumps(dict(time=utc(), event=event, **fields)) + '\n')
        print(event, flush=True)
        if event == 'selection-writable-attach-fallback':
            self.outcome['selection_writable_replay_attempted'] = True
        if event == 'time-machine-progress':
            status = status_observations(fields.get('native_status', ''))
            with (EVIDENCE / 'status-samples.jsonl').open('a') as f:
                f.write(json.dumps(dict(monotonic_ns=time.monotonic_ns(), wall_ns=time.time_ns(),
                                        command_exit=fields.get('exit'), **status)) + '\n')
            self.sample_geometry('progress')

    def sample_geometry(self, stage):
        output, code = self.cmd.run([sys.executable, Path(__file__).with_name('sample_geometry.py'),
                                    self.share, WORK], timeout=5, diagnostic=True)
        result = dict(stage=stage, command_exit=code, observation=None)
        if code == 0:
            try:
                result['observation'] = json.loads(output)
            except (ValueError, TypeError):
                result['parse_error'] = True
        with (EVIDENCE / 'geometry-samples.jsonl').open('a') as f:
            f.write(json.dumps(result) + '\n')

    def collect_client_outcome(self):
        # Small attempt-window backupd-only query. The inherited broad private
        # log remains retained, but is never loaded wholesale by this parser.
        result = dict(attempt=1, log_scope='unknown', collection_exit=None,
                      collection_timeout=None, boundary_margin_seconds=1,
                      clock_domain='runner_local_wall_filter_not_connection_attribution',
                      observations=dict(parse='not-run'))
        if self.attempt_start_wall is not None and self.attempt_end_wall is not None:
            start = time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(self.attempt_start_wall - 1))
            end = time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(self.attempt_end_wall + 1))
            _, code = self.cmd.run(['/usr/bin/log', 'show', '--style', 'json', '--start', start,
                '--end', end, '--info', '--debug', '--predicate',
                'process == "backupd" OR process == "backupd-helper"'],
                timeout=60, diagnostic=True, capture=False)
            path = EVIDENCE / f'{self.cmd.seq:04d}-log.log'
            result.update(log_scope='attempt-window', collection_exit=code,
                          collection_timeout=code == 124, observations=collect('unified', path))
        self.save('client-outcome.json', result)

    def start_capture(self):
        meta = EVIDENCE / 'capture'
        meta.mkdir(mode=0o700)
        self.capture_log = (EVIDENCE / 'capture-private.log').open('xb', buffering=0)
        argv = [str(BIN / 'passive-capture'), 'capture', 'lo0', '1445', str(8 * 2**20),
                str(WORK / 'private-traffic.pcap'), str(meta), str(32 * 2**30),
                '1800', str(20 * 2**30)]
        self.save('capture-command.json', dict(argv=argv, started=utc(),
                  raw_policy='authenticated encrypted retention only; no plaintext upload',
                  offline_analysis='deferred until encrypted retention and resource admission'))
        self.capture = subprocess.Popen(argv, stdout=self.capture_log, stderr=self.capture_log)
        deadline = time.monotonic() + 15
        ready = meta / 'ready.json'
        while time.monotonic() < deadline:
            if self.capture.poll() is not None:
                raise RuntimeError('passive capture exited before readiness')
            if ready.exists():
                # Producer atomically publishes only after activation + BIOCGBLEN.
                self.capture_ready = json.loads(ready.read_text())
                if (type(self.capture_ready.get('effective_buffer_bytes')) is not int or
                        self.capture_ready['effective_buffer_bytes'] <= 0 or
                        self.capture_ready.get('snaplen') != 262144):
                    raise RuntimeError('capture readiness lacks effective allocation/full snaplen')
                self.event('capture-ready', pid=self.capture.pid)
                return
            time.sleep(.1)
        raise RuntimeError('passive capture readiness not observed')

    def run(self):
        try:
            self.outcome['stage'] = 'setup'
            self.platform()
            self.start_capture()
            self.start_services(fresh=True)
            self.start_daemon('initialize')
            self.mount_share()
            self.configure_destination()
            self.sample_geometry('before')
            proof = self.create_tree()
            self.check_exclusions()
            self.outcome['stage'] = 'time_machine'
            self.attempt_start_wall = time.time()
            self.outcome['start_monotonic_ns'] = time.monotonic_ns()
            self.start_backup('baseline')
            process = self.backup[0]
            try:
                self.complete_backup('baseline')
                self.outcome['tm_command_completed'] = True
            finally:
                self.attempt_end_wall = time.time()
                self.outcome['end_monotonic_ns'] = time.monotonic_ns()
                self.outcome['tm_command_exit'] = process.poll()
                self.sample_geometry('after_command')
            self.outcome['stage'] = 'backup_selection'
            self.detach_clients()
            self.mount_share()
            selected = self.remote_backup('baseline')
            self.outcome['completed_backup_selected'] = True
            self.outcome['stage'] = 'tree_presence'
            self.tree_in_backup(selected, proof.relative_to('/'))
            self.outcome['tree_directory_present'] = True
            self.outcome['stage'] = 'detach_and_store_size'
            self.detach_clients()
            size, _ = self.cmd.run(['/usr/bin/du', '-sk', WORK / 'objects'], timeout=300)
            result = dict(store_kilobytes=int(size.split()[0]))
            self.outcome.update(run_stage_completed=True, stage='completed')
            return result
        finally:
            self.save('workload-outcome.json', dict(time=utc(), **self.outcome))

    def finish(self):
        cleanup_ok = False
        try:
            super().finish()  # Joins application PTY reader before counting.
            cleanup_ok = True
        finally:
            self.save('cleanup-outcome.json', dict(completed=cleanup_ok))
            errors = {}
            for text in ('short client packet header', 'invalid transport format'):
                # Read by line, never load an unbounded application log.
                count = 0
                for path in EVIDENCE.glob('application-*.log'):
                    with path.open('rb') as f:
                        count += sum(line.count(text.encode()) for line in f)
                errors[text] = count
            joined = bool(self.daemon and not self.daemon.reader.is_alive())
            self.save('framing-error-counts.json', dict(time=utc(), errors=errors,
                      counted_after_reader_join=joined, server_log_scope='run-only'))
            capture_ok = False
            if self.capture is not None:
                was_alive = self.capture.poll() is None
                forced = False
                if was_alive:
                    self.capture.send_signal(signal.SIGINT)
                try:
                    code = self.capture.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    forced = True
                    self.capture.kill()
                    code = self.capture.wait(timeout=5)
                self.capture_log.close()
                summary_exists = (EVIDENCE / 'capture/summary.json').is_file()
                self.save('measurement.json', dict(time=utc(), capture_exit=code,
                          capture_alive_before_stop=was_alive, forced_capture_stop=forced,
                          capture_ready_observed=self.capture_ready is not None,
                          capture_summary_present=summary_exists,
                          offline_analysis='not_run_pending_authenticated_retention',
                          whole_stream_coverage_proven=False,
                          semantic_correctness_proven=False,
                          caveat='Capture counters alone do not prove stream coverage; no online parser.'))
                capture_ok = bool(was_alive and code == 0 and not forced and summary_exists)
            else:
                self.save('measurement.json', dict(capture_ready_observed=False,
                          capture_summary_present=False, capture_exit=None,
                          offline_analysis='not_run_pending_authenticated_retention',
                          whole_stream_coverage_proven=False, semantic_correctness_proven=False))
            self.collect_client_outcome()
            if not capture_ok:
                raise RuntimeError('capture incomplete; workload outcome is separate')
            if any(errors.values()):
                raise RuntimeError('historical framing symptom observed; see separate counters')


if __name__ == '__main__':
    def deadline(signum, frame):
        raise TimeoutError('diagnostic deadline reached')
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(max(1, int(os.environ['MAC_DEADLINE_EPOCH']) - int(time.time())))
    Diagnostic().execute()
