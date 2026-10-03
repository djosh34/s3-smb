#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Ordinary local TCP throughput calibration; never an SMB/Time Machine control."""
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import struct
import subprocess
import sys
import threading
import time

TOTAL = 19_000_000_000
SECONDS = 300
PORT = 28464


def save(path, value):
    with path.open('x') as f:
        json.dump(value, f)
        f.write('\n')


def transfer(total, seconds, port, alive=lambda: True):
    """One sender, continuously reading receiver, orderly bidirectional FINs."""
    result = {}
    payload = bytes(1024 * 1024)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', port))
        listener.listen(1)
        listener.settimeout(20)
        port = listener.getsockname()[1]

        def receive():
            try:
                with listener.accept()[0] as connection:
                    connection.settimeout(30)
                    digest = hashlib.sha256()
                    count = 0
                    while True:
                        data = connection.recv(1024 * 1024)
                        if not data:
                            break
                        digest.update(data)
                        count += len(data)
                    result['received_bytes'] = count
                    result['receiver_hash'] = digest.digest()
                    connection.sendall(struct.pack('!Q', count))
                    connection.shutdown(socket.SHUT_WR)
            except Exception:
                result['receiver_failed'] = True

        reader = threading.Thread(target=receive, daemon=True)
        reader.start()
        start = time.monotonic()
        count = 0
        digest = hashlib.sha256()
        try:
            with socket.create_connection(('127.0.0.1', port), timeout=20) as connection:
                connection.settimeout(30)
                while count < total:
                    if not alive():
                        raise RuntimeError('capture stopped')
                    block = payload[:min(len(payload), total-count)]
                    connection.sendall(block)
                    digest.update(block)
                    count += len(block)
                    delay = start + seconds * count / total - time.monotonic()
                    if delay > 0:
                        time.sleep(delay)
                connection.shutdown(socket.SHUT_WR)
                reply = bytearray()
                while True:
                    block = connection.recv(16)
                    if not block:
                        break
                    reply.extend(block)
                    if len(reply) > 8:
                        raise RuntimeError('unexpected reply length')
            reader.join(35)
            matched = (not reader.is_alive() and not result.get('receiver_failed') and
                       result.get('received_bytes') == total == count and
                       result.get('receiver_hash') == digest.digest() and
                       bytes(reply) == struct.pack('!Q', total))
            return dict(workload_valid=matched, sent_bytes=count,
                        received_bytes=result.get('received_bytes', 0),
                        hashes_equal=result.get('receiver_hash') == digest.digest(),
                        reply_bytes=len(reply), elapsed_seconds=time.monotonic()-start)
        finally:
            reader.join(35)


def run(root):
    evidence = root / 'evidence'
    capture = evidence / 'capture'
    capture.mkdir(mode=0o700)
    if sys.platform != 'darwin' or os.geteuid() != 0:
        raise RuntimeError('Darwin root required')
    if shutil.disk_usage(root).free < 56 * 1024**3:
        raise RuntimeError('insufficient disk reservation')
    # Read-only provenance; no TCP, signing, kernel-security or sysctl mutations.
    with (evidence / 'platform.log').open('xb') as log:
        for argv in (['sw_vers'], ['uname', '-a'],
                     ['sysctl', 'debug.bpf_bufsize', 'debug.bpf_maxbufsize', 'debug.bpf_bufsize_cap']):
            subprocess.run(argv, stdout=log, stderr=log, check=False, timeout=10)
    provenance = dict(
        helper_sha256=hashlib.sha256((root/'passive-capture').read_bytes()).hexdigest(),
        checker_sha256=hashlib.sha256((root/'calibration-audit').read_bytes()).hexdigest(),
        harness_revision=subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip(),
        os_version=subprocess.check_output(['sw_vers', '-productVersion'], text=True).strip(),
        os_build=subprocess.check_output(['sw_vers', '-buildVersion'], text=True).strip())
    save(evidence / 'provenance.json', provenance)
    process = None
    workload = dict(workload_valid=False)
    exit_code = -1
    with (evidence / 'capture.log').open('xb') as log:
        try:
            process = subprocess.Popen([str(root / 'passive-capture'), 'capture', 'lo0', str(PORT),
                                        str(32*1024**2), str(root / 'private-traffic.pcap'), str(capture),
                                        str(24*1024**3), '420', str(24*1024**3)], stdout=log, stderr=log)
            until = time.monotonic()+15
            while not (capture / 'ready.json').exists():
                if process.poll() is not None or time.monotonic() > until:
                    raise RuntimeError('capture not ready')
                time.sleep(.05)
            ready = json.loads((capture / 'ready.json').read_text())
            if not 4096 <= ready['effective_buffer_bytes'] <= 32*1024**2 or ready['snaplen'] != 262144:
                raise RuntimeError('unknown capture allocation or snaplen')
            workload = transfer(TOTAL, SECONDS, PORT, lambda: process.poll() is None)
            # Application sockets have closed; capture drain observes the terminal ACKs.
            time.sleep(1)
        finally:
            if process is not None:
                if process.poll() is None:
                    process.send_signal(signal.SIGINT)
                try:
                    exit_code = process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            save(evidence / 'workload.json', dict(workload, capture_exit=exit_code))
    return bool(workload['workload_valid'] and exit_code == 0)


SCHEMAS = {
    'ready': {'requested_buffer_bytes': int, 'effective_buffer_bytes': int, 'snaplen': int, 'datalink': int},
    'summary': {'capture_health_valid': bool, 'completeness_proven': bool, 'error_code': int, 'drained': bool,
                'requested_buffer_bytes': int, 'effective_buffer_bytes': int, 'packets': int,
                'packet_bytes': int, 'pcap_bytes': int, 'truncated_packets': int, 'received': int,
                'dropped': int, 'interface_dropped': int, 'elapsed_seconds': float,
                'dispatch_seconds': float, 'max_dispatch_seconds': float, 'flush_seconds': float},
    'workload': {'workload_valid': bool, 'sent_bytes': int, 'received_bytes': int, 'hashes_equal': bool,
                 'reply_bytes': int, 'elapsed_seconds': float, 'capture_exit': int},
    'audit': {'complete': bool, 'decode_or_order_error': bool, 'packets': int, 'client_bytes': int,
              'server_bytes': int, 'client_payload_span_seconds': float},
}


def numeric(path, schema):
    if path.stat().st_size > 16384:
        raise ValueError('metadata too large')
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError('metadata object required')
    result = {}
    for name, kind in schema.items():
        item = value[name]
        if type(item) is not kind or (kind is float and not math.isfinite(item)):
            raise ValueError('metadata type rejected')
        if kind in (int, float) and not -1 <= item <= 2**63:
            raise ValueError('metadata number rejected')
        result[name] = item
    return result


def publish(root, public):
    result = dict(calibration_valid=False, time_machine_control=False, smb_control=False)
    try:
        evidence = root / 'evidence'
        values = {name: numeric((evidence / 'capture' if name in ('ready', 'summary') else evidence) /
                               (name+'.json'), schema) for name, schema in SCHEMAS.items()}
        path = evidence / 'provenance.json'
        if path.stat().st_size > 16384:
            raise ValueError('provenance too large')
        provenance = json.loads(path.read_text())
        checked = {}
        for key, pattern in (('helper_sha256', r'[0-9a-f]{64}'), ('checker_sha256', r'[0-9a-f]{64}'),
                             ('harness_revision', r'[0-9a-f]{40}'), ('os_version', r'[0-9]{1,2}(\.[0-9]{1,2}){1,2}'),
                             ('os_build', r'[0-9]{1,2}[A-Z][0-9]{1,6}[a-z]?')):
            value = provenance[key]
            if type(value) is not str or not re.fullmatch(pattern, value):
                raise ValueError('provenance field rejected')
            checked[key] = value
        summary, workload, audit = (values[x] for x in ('summary', 'workload', 'audit'))
        ready = values['ready']
        valid = (0 < ready['effective_buffer_bytes'] <= 32*1024**2 and ready['snaplen'] == 262144 and
                 ready['effective_buffer_bytes'] == summary['effective_buffer_bytes'] and
                 summary['capture_health_valid'] and not summary['completeness_proven'] and
                 summary['drained'] and summary['packets'] > 0 and summary['received'] > 0 and
                 summary['error_code'] == summary['dropped'] == summary['interface_dropped'] ==
                 summary['truncated_packets'] == workload['capture_exit'] == 0 and
                 workload['workload_valid'] and workload['hashes_equal'] and audit['complete'] and
                 not audit['decode_or_order_error'] and audit['packets'] == summary['packets'] and
                 workload['sent_bytes'] == workload['received_bytes'] == audit['client_bytes'] == TOTAL and
                 workload['reply_bytes'] == audit['server_bytes'] == 8 and
                 SECONDS-2 <= audit['client_payload_span_seconds'] <= SECONDS+20 and
                 (public / 'capture.tar.gz.cms').stat().st_size > 0 and
                 (root / 'encryption-success').stat().st_size == 9 and
                 (root / 'encryption-success').read_bytes() == b'complete\n')
        result.update(values, calibration_valid=valid, provenance=checked)
    except (OSError, ValueError, TypeError, KeyError):
        pass  # Fixed failure only; never exception strings or private input values.
    save(public / 'calibration.json', result)
    return result['calibration_valid']


if __name__ == '__main__':
    os.umask(0o077)
    if sys.argv[1] == 'run':
        def timeout(signum, frame):
            raise TimeoutError('calibration deadline')
        signal.signal(signal.SIGALRM, timeout)
        signal.alarm(400)
        ok = run(Path(sys.argv[2]))
    elif sys.argv[1] == 'publish':
        ok = publish(Path(sys.argv[2]), Path(sys.argv[3]))
    else:
        raise SystemExit(2)
    raise SystemExit(0 if ok else 1)
