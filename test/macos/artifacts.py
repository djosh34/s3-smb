#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Task-scoped private evidence handoff, then full verification as uploader."""
import hashlib
import json
import os
from pathlib import Path
import stat
import sys

INVENTORY = 'artifact-inventory.json'


def entries(root):
    if root.is_symlink() or not root.is_dir():
        raise RuntimeError('evidence must be its own real directory')
    result = []
    for directory, dirs, files in os.walk(root, followlinks=False):
        for name in sorted(dirs + files):
            path = Path(directory) / name
            mode = path.lstat().st_mode
            if not (stat.S_ISDIR(mode) or stat.S_ISREG(mode)):
                raise RuntimeError(f'non-regular evidence entry: {path}')
            result.append(path)
    return result


def checksum(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(4 * 1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def handoff(root, uid, gid):
    paths = entries(root)
    inventory = root / INVENTORY
    if inventory.exists():
        raise RuntimeError('evidence was already finalized')
    records = []
    for path in paths:
        s = path.stat()
        directory = stat.S_ISDIR(s.st_mode)
        record = dict(path=str(path.relative_to(root)), directory=directory)
        if not directory:
            record.update(bytes=s.st_size, sha256=checksum(path))
        records.append(record)
    inventory.write_text(json.dumps(dict(uid=uid, gid=gid, entries=records), indent=2) + '\n')
    # No broad chmod/chown: only this newly created evidence tree, no links.
    # Original private ownership semantics are preserved for the new reader.
    for path in [*paths, inventory, root]:
        os.chmod(path, 0o700 if path.is_dir() else 0o600)
        os.chown(path, uid, gid)


def verify(root):
    inventory = root / INVENTORY
    info = json.loads(inventory.read_text())
    if (os.geteuid(), os.getegid()) != (info['uid'], info['gid']):
        raise RuntimeError('verification must run as actual artifact uploader identity')
    paths = entries(root)
    expected = {row['path']: row for row in info['entries']}
    actual = {str(p.relative_to(root)): p for p in paths}
    if set(actual) != set(expected) | {INVENTORY}:
        raise RuntimeError('evidence inventory incomplete or changed after handoff')
    for path in [root, *paths]:
        s = path.stat()
        if (s.st_uid, s.st_gid) != (info['uid'], info['gid']):
            raise RuntimeError(f'evidence ownership not handed off: {path}')
        if stat.S_IMODE(s.st_mode) != (0o700 if path.is_dir() else 0o600):
            raise RuntimeError(f'evidence is not private to uploader: {path}')
    for name, row in expected.items():
        path = actual[name]
        if row['directory'] != path.is_dir():
            raise RuntimeError(f'evidence type changed: {path}')
        if not row['directory'] and (path.stat().st_size != row['bytes'] or checksum(path) != row['sha256']):
            raise RuntimeError(f'evidence content changed: {path}')
    print(f'Uploader uid={os.geteuid()} gid={os.getegid()} read and verified every artifact: {len(expected)} entries')


if __name__ == '__main__':
    mode, path, *identity = sys.argv[1:]
    root = Path(path).absolute()
    if mode == 'handoff' and len(identity) == 2:
        handoff(root, *map(int, identity))
    elif mode == 'verify' and not identity:
        verify(root)
    else:
        raise SystemExit('usage: artifacts.py handoff PATH UID GID | verify PATH')
