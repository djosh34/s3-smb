#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Conservative fresh-space gate before ciphertext creation; no payload reads."""
import json
from pathlib import Path
import shutil
import sys


def check(raw, logs, output, reserve):
    raw, logs, output = Path(raw), Path(logs), Path(output)
    entries = list(logs.rglob('*'))
    if raw.exists():
        entries.append(raw)
    if len(entries) > 10000 or any(p.is_symlink() for p in entries):
        raise RuntimeError('retention member policy failed')
    members = [p for p in entries if p.is_file()]
    # One percent plus1MiB covers gzip expansion, tar headers/padding and CMS
    # overhead for this bounded archive. No favorable compression is assumed.
    source_bytes = sum(p.stat().st_size for p in members)
    upper = (source_bytes * 101 + 99) // 100 + len(entries) * 4096 + 1024 * 1024
    free = shutil.disk_usage(output).free
    result = dict(source_bytes=source_bytes, members=len(members),
                  ciphertext_upper_bound_bytes=upper, free_bytes=free,
                  reserve_bytes=reserve, admitted=free >= upper + reserve,
                  favorable_compression_assumed=False)
    (output / 'retention-space.json').write_text(json.dumps(result) + '\n')
    return result['admitted']


if __name__ == '__main__':
    raise SystemExit(0 if check(*sys.argv[1:4], int(sys.argv[4])) else 2)
