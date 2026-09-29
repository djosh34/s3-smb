#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Bound the build/fetch/install process group by the shared acceptance deadline."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time


def run_until(argv, deadline, evidence, grace=10):
    started = time.time()
    process = subprocess.Popen(argv, start_new_session=True)
    timed_out = False
    code = None
    try:
        code = process.wait(timeout=max(0, deadline - time.time()))
    except subprocess.TimeoutExpired:
        timed_out = True
    finally:
        # Kill the task group, not merely its shell. This also handles a shell
        # that exited while a background compiler/download process survived it.
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            pass
        # The leader can exit before its children. Always finish the group.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait(timeout=5)
        evidence.write_text(json.dumps(dict(started=started, ended=time.time(), deadline=deadline,
                                            timed_out=timed_out, command_exit=process.returncode)) + '\n')
    return 124 if timed_out else code


if __name__ == '__main__':
    deadline, evidence, *command = sys.argv[1:]
    sys.exit(run_until(command, float(deadline), Path(evidence)))
