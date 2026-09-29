#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Small independent reference for the deliberately created acceptance tree only."""
import hashlib
import json
from pathlib import Path
import stat


def manifest(root, output):
    root = Path(root)
    counts = dict(entries=0, files=0, bytes=0)
    with open(output, 'x') as out:
        def visit(path):
            mode = path.lstat().st_mode
            row = dict(path=str(path.relative_to(root)))
            if stat.S_ISDIR(mode):
                row['type'] = 'directory'
            elif stat.S_ISREG(mode):
                digest = hashlib.sha256()
                size = 0
                with path.open('rb') as source:
                    for block in iter(lambda: source.read(1024 * 1024), b''):
                        digest.update(block)
                        size += len(block)
                row.update(type='file', bytes=size, sha256=digest.hexdigest())
                counts['files'] += 1
                counts['bytes'] += size
            else:
                # The fixture creates only regular files/directories. A symlink
                # replacing either must fail, never lead outside the test tree.
                row['type'] = 'unexpected:' + str(stat.S_IFMT(mode))
            out.write(json.dumps(row, sort_keys=True) + '\n')
            counts['entries'] += 1
            if stat.S_ISDIR(mode):
                for child in sorted(path.iterdir()):
                    visit(child)
        visit(root)
    return counts


def compare(expected, actual, differences):
    def load(path):
        rows = [json.loads(line) for line in Path(path).read_text().splitlines()]
        result = {row['path']: row for row in rows}
        if not rows or len(result) != len(rows):
            raise RuntimeError('empty or duplicate created-tree reference')
        return result
    left, right = load(expected), load(actual)
    count = 0
    with open(differences, 'x') as out:
        for path in sorted(left.keys() | right.keys()):
            if left.get(path) != right.get(path):
                out.write(json.dumps(dict(path=path, expected=left.get(path), actual=right.get(path)), sort_keys=True) + '\n')
                count += 1
    if count:
        raise RuntimeError(f'{count} created-tree differences; see {differences}')
