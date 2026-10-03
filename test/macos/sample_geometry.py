#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Run as a timeout-bounded subprocess: read only numeric SMB/inner geometry."""
import json
import sys
import time
from apple_tm_measure import capacity, geometry


def sample(share, work):
    result = dict(monotonic_ns=time.monotonic_ns(), wall_ns=time.time_ns(),
                  share_capacity=None, backing_capacity=None,
                  capacity_error=False, inner_geometry=None)
    try:
        result['share_capacity'] = capacity(share)
        result['backing_capacity'] = capacity(work)
    except OSError:
        result['capacity_error'] = True
    result['inner_geometry'] = geometry(share)
    result['end_monotonic_ns'] = time.monotonic_ns()
    return result


if __name__ == '__main__':
    print(json.dumps(sample(*sys.argv[1:]), allow_nan=False))
