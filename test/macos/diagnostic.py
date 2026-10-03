#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Ordinary public rc.7 Time Machine backup; capture only, no live/offline parser."""
import json
import os
from pathlib import Path
import signal
import subprocess
import time

from acceptance import Acceptance, WORK, EVIDENCE, BIN
from native import utc


class Diagnostic(Acceptance):
    def __init__(self):
        super().__init__()
        self.capture = None
        self.capture_log = None
        self.capture_ready = None
        self.outcome = dict(run_stage_completed=False, tm_command_completed=False,
                            tm_command_exit=None, completed_backup_selected=False,
                            tree_directory_present=False, restore_performed=False,
                            content_hash_verified=False, stage='not_started')

    def event(self, event, **fields):
        # Native status/paths remain in private evidence, never the Actions log.
        with (EVIDENCE / 'acceptance.jsonl').open('a') as f:
            f.write(json.dumps(dict(time=utc(), event=event, **fields)) + '\n')
        print(event, flush=True)

    def start_capture(self):
        meta = EVIDENCE / 'capture'
        meta.mkdir(mode=0o700)
        self.capture_log = (EVIDENCE / 'capture-private.log').open('xb', buffering=0)
        argv = [str(BIN / 'passive-capture'), 'lo0', '1445', str(8 * 2**20),
                str(WORK / 'private-traffic.pcap'), str(meta), str(48 * 2**30),
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
            proof = self.create_tree()
            self.check_exclusions()
            self.outcome['stage'] = 'time_machine'
            self.start_backup('baseline')
            process = self.backup[0]
            try:
                self.complete_backup('baseline')
                self.outcome['tm_command_completed'] = True
            finally:
                self.outcome['tm_command_exit'] = process.poll()
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
                      counted_after_reader_join=joined))
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
                if not (was_alive and code == 0 and not forced and summary_exists):
                    raise RuntimeError('capture incomplete; workload outcome is separate')
            if any(errors.values()):
                raise RuntimeError('historical framing symptom observed; see separate counters')


if __name__ == '__main__':
    def deadline(signum, frame):
        raise TimeoutError('diagnostic deadline reached')
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(max(1, int(os.environ['MAC_DEADLINE_EPOCH']) - int(time.time())))
    Diagnostic().execute()
