#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded transport control, not a Time Machine acceptance test.

Write 24 GiB over four file handles for at least six minutes. Reuse four 1 GiB
files so full packet capture, not sparsebundle allocation, dominates disk use.
No payload, credentials, or SMB session material is printed.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import random
import threading
import time


def run(root, total_mib=24576, seconds=384, workers=4, slot_mib=1024):
    if total_mib <= 0 or workers <= 0 or seconds < 0 or slot_mib <= 0:
        raise ValueError('positive sizes/workers and nonnegative duration required')
    if total_mib % workers or (total_mib // workers) % slot_mib:
        raise ValueError('each worker must write a whole number of file passes')
    root = Path(root)
    root.mkdir(exist_ok=False)
    lock = threading.Lock()
    started = time.monotonic()

    def event(**fields):
        with lock:
            print(json.dumps(dict(seconds=round(time.monotonic() - started, 3), **fields)), flush=True)

    def writer(index):
        # Different deterministic incompressible blocks distinguish payload from
        # zero runs. Alternating complete file passes preserve final hash checks.
        block = random.Random(6400 + index).randbytes(2**20)
        zero = bytes(2**20)
        path = root / f'writer-{index}.bin'
        amount = total_mib // workers
        interval = seconds / amount
        expected = None
        with path.open('xb', buffering=0) as out:
            for count in range(amount):
                offset = count % slot_mib
                if offset == 0:
                    out.seek(0)
                    expected = hashlib.sha256()
                payload = zero if (count // slot_mib) % 2 == 0 else block
                view = memoryview(payload)
                while view:
                    written = out.write(view)
                    if not written:
                        raise IOError('zero-length write')
                    view = view[written:]
                expected.update(payload)
                if (count + 1) % 64 == 0 or count + 1 == amount:
                    before = time.monotonic()
                    os.fsync(out.fileno())
                    event(event='progress', worker=index, mib=count + 1,
                          fsync_seconds=round(time.monotonic() - before, 4))
                # Rate-limit at the application boundary, never alter TCP.
                delay = started + (count + 1) * interval - time.monotonic()
                if delay > 0:
                    time.sleep(delay)
        actual = hashlib.sha256()
        with path.open('rb', buffering=0) as inp:
            while chunk := inp.read(2**20):
                actual.update(chunk)
        if actual.digest() != expected.digest():
            raise AssertionError(f'readback mismatch worker {index}')
        result = dict(file=path.name, size=slot_mib * 2**20, sha256=actual.hexdigest())
        event(event='verified', worker=index, **result)
        return result

    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        results = list(pool.map(writer, range(workers)))
    event(event='control-write-complete', total_mib=total_mib, workers=workers,
          minimum_seconds=seconds, files=results)
    return results


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root')
    parser.add_argument('--mib', type=int, default=24576)
    parser.add_argument('--seconds', type=float, default=384)
    parser.add_argument('--workers', type=int, default=4)
    parser.add_argument('--slot-mib', type=int, default=1024)
    args = parser.parse_args()
    run(args.root, args.mib, args.seconds, args.workers, args.slot_mib)
