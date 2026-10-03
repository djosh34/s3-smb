#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Branch-only real rc.7 Time Machine format/backup, no export or recovery."""
import json
import os
import re
from pathlib import Path
import shutil
import signal
import subprocess
import threading
import time

from acceptance import Acceptance, WORK, EVIDENCE, PROOF
from framing_capture import audit_pcap
from native import utc


class Diagnostic(Acceptance):
    def __init__(self):
        super().__init__()
        self.capture = None
        self.capture_log = None
        self.monitor_stop = threading.Event()
        self.monitor = None
        self.capture_health = []

    def start_capture(self):
        self.capture_log = (EVIDENCE / 'tcpdump-stderr.log').open('xb', buffering=0)
        argv = ['/usr/sbin/tcpdump', '-i', 'lo0', '-nn', '-s', '0', '-B', '131072', '-U',
                '-w', str(WORK / 'private-traffic.pcap'), 'tcp port 1445']
        self.save('capture-command.json', dict(argv=argv, started=utc(),
                  raw_policy='private scratch only; never uploaded', export='numeric metadata only'))
        self.capture = subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=self.capture_log)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if self.capture.poll() is not None:
                raise RuntimeError('tcpdump exited before readiness')
            if b'listening on lo0' in (EVIDENCE / 'tcpdump-stderr.log').read_bytes():
                break
            time.sleep(.1)
        else:
            raise RuntimeError('tcpdump readiness not observed')
        self.event('capture-ready', pid=self.capture.pid)
        self.monitor = threading.Thread(target=self.monitor_capture, daemon=True)
        self.monitor.start()

    def monitor_capture(self):
        with (EVIDENCE / 'capture-health.jsonl').open('w') as log:
            while not self.monitor_stop.wait(5):
                raw = WORK / 'private-traffic.pcap'
                sample = dict(time=utc(), exit=self.capture.poll(), free_bytes=shutil.disk_usage(WORK).free,
                              raw_bytes=raw.stat().st_size if raw.exists() else 0)
                log.write(json.dumps(sample) + '\n')
                log.flush()
                if sample['exit'] is not None or sample['free_bytes'] < 20 * 2**30:
                    self.capture_health.append(sample)

    def run(self):
        self.platform()
        self.start_capture()
        self.start_services(fresh=True)
        self.start_daemon('initialize')
        self.mount_share()
        self.configure_destination()
        proof = self.create_tree()
        self.check_exclusions()
        self.start_backup('baseline')
        self.complete_backup('baseline')
        self.detach_clients()
        self.mount_share()
        selected = self.remote_backup('baseline')
        self.tree_in_backup(selected, proof.relative_to('/'))
        self.detach_clients()
        size, _ = self.cmd.run(['/usr/bin/du', '-sk', WORK / 'objects'], timeout=300)
        return dict(baseline=selected.name, store_kilobytes=int(size.split()[0]))

    def finish(self):
        try:
            super().finish()  # Joins the application PTY reader before counting.
        finally:
            errors = {}
            for text in ('short client packet header', 'invalid transport format'):
                errors[text] = sum(p.read_bytes().count(text.encode()) for p in EVIDENCE.glob('application-*.log'))
            joined = bool(self.daemon and not self.daemon.reader.is_alive())
            self.save('framing-error-counts.json', dict(time=utc(), errors=errors, counted_after_reader_join=joined))
            if self.capture is not None:
                was_alive = self.capture.poll() is None
                if was_alive:
                    self.capture.send_signal(signal.SIGINT)
                try:
                    code = self.capture.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    self.capture.kill()
                    code = self.capture.wait(timeout=5)
                self.monitor_stop.set()
                if self.monitor:
                    self.monitor.join(timeout=10)
                self.capture_log.close()
                self.save('capture-stop.json', dict(time=utc(), alive_before_stop=was_alive,
                          exit=code, unhealthy_samples=self.capture_health))
                raw = WORK / 'private-traffic.pcap'
                if raw.exists():
                    audit_pcap(raw, EVIDENCE / 'framing')
                summary_path = EVIDENCE / 'framing/capture-summary.json'
                summary = json.loads(summary_path.read_text()) if summary_path.exists() else {}
                drops = re.search(r'(\d+) packets dropped by kernel', (EVIDENCE / 'tcpdump-stderr.log').read_text())
                valid = bool(was_alive and code == 0 and not self.capture_health and drops and int(drops[1]) == 0
                             and summary.get('observed_prefix_contiguous') and joined)
                self.save('measurement.json', dict(time=utc(), observed_prefix_valid=valid,
                          complete_connection_capture=summary.get('complete_reassembly', False),
                          tcpdump_exit=code, kernel_drops=int(drops[1]) if drops else None,
                          caveat='TCP framing only, not a proof of SMB semantic correctness; raw scratch is ephemeral'))
                if not valid:
                    raise RuntimeError('measurement incomplete; workload result is separate from capture validity')
            if any(errors.values()):
                raise RuntimeError('exact historical framing symptom observed; see framing-error-counts.json')


if __name__ == '__main__':
    def deadline(signum, frame):
        raise TimeoutError('diagnostic deadline reached')
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(max(1, int(os.environ['MAC_DEADLINE_EPOCH']) - int(time.time())))
    Diagnostic().execute()
