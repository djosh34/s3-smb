#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Complete tree evidence; no fixture-only filters or silent unreadable entries."""
import ctypes
import hashlib
import json
import os
from pathlib import Path
import stat
import sys


def digest_file(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(4 * 1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def acl_text(path):
    if sys.platform != 'darwin':
        return None  # Portable helper tests only; acceptance requires Darwin.
    lib = ctypes.CDLL('/usr/lib/libSystem.B.dylib', use_errno=True)
    lib.acl_get_link_np.argtypes = [ctypes.c_char_p, ctypes.c_int]
    lib.acl_get_link_np.restype = ctypes.c_void_p
    lib.acl_to_text.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ssize_t)]
    lib.acl_to_text.restype = ctypes.c_void_p
    lib.acl_free.argtypes = [ctypes.c_void_p]
    acl = lib.acl_get_link_np(os.fsencode(path), 0x100)  # ACL_TYPE_EXTENDED
    if not acl:
        raise OSError(ctypes.get_errno(), 'acl_get_link_np', str(path))
    try:
        length = ctypes.c_ssize_t()
        text = lib.acl_to_text(acl, ctypes.byref(length))
        if not text:
            raise OSError(ctypes.get_errno(), 'acl_to_text', str(path))
        try:
            return ctypes.string_at(text, length.value).decode('utf-8')
        finally:
            lib.acl_free(text)
    finally:
        lib.acl_free(acl)


def manifest(root, output):
    root = Path(root)
    seen_links = {}
    counts = {'entries': 0, 'files': 0, 'bytes': 0}
    with open(output, 'x') as out:
        def visit(path):
            s = path.lstat()
            rel = str(path.relative_to(root))
            kind = stat.S_IFMT(s.st_mode)
            row = dict(path=rel, type=kind, mode=stat.S_IMODE(s.st_mode),
                       uid=s.st_uid, gid=s.st_gid, flags=getattr(s, 'st_flags', 0),
                       mtime_ns=s.st_mtime_ns, birthtime=getattr(s, 'st_birthtime', None),
                       atime_ns=s.st_atime_ns, ctime_ns=s.st_ctime_ns,
                       inode=s.st_ino, device=s.st_dev, nlink=s.st_nlink,
                       acl=acl_text(path), xattrs={})
            for name in sorted(os.listxattr(path, follow_symlinks=False)):
                value = os.getxattr(path, name, follow_symlinks=False)
                row['xattrs'][name] = {'bytes': len(value), 'sha256': hashlib.sha256(value).hexdigest()}
            if stat.S_ISREG(s.st_mode):
                row.update(size=s.st_size, sha256=digest_file(path))
                counts['files'] += 1
                counts['bytes'] += s.st_size
                key = (s.st_dev, s.st_ino)
                row['hardlink_to'] = seen_links.setdefault(key, rel)
            elif stat.S_ISLNK(s.st_mode):
                row['target'] = os.readlink(path)
            elif not (stat.S_ISDIR(s.st_mode) or stat.S_ISFIFO(s.st_mode)):
                # Record device/socket identity rather than silently omit it.
                row['rdev'] = s.st_rdev
            # Reading evidence must not race changing file contents/metadata.
            after = path.lstat()
            if (s.st_size, s.st_mtime_ns, s.st_ctime_ns) != (after.st_size, after.st_mtime_ns, after.st_ctime_ns):
                raise RuntimeError(f'manifest source changed while reading: {path}')
            out.write(json.dumps(row, sort_keys=True) + '\n')
            counts['entries'] += 1
            if stat.S_ISDIR(s.st_mode):
                for child in sorted(path.iterdir(), key=lambda p: os.fsencode(p.name)):
                    visit(child)
        visit(root)
    return counts


# Physical allocation, inode/device numbers and access/change times necessarily
# differ after native restore; retain them in raw evidence, not as data equality.
OBSERVATIONAL = {'inode', 'device', 'nlink', 'atime_ns', 'ctime_ns'}


def compare(expected, actual, differences):
    import itertools
    count = 0
    with open(expected) as a, open(actual) as b, open(differences, 'x') as out:
        for left, right in itertools.zip_longest(a, b):
            l = json.loads(left) if left else None
            r = json.loads(right) if right else None
            def stable(row):
                return {k: v for k, v in row.items() if k not in OBSERVATIONAL} if row else None
            if stable(l) != stable(r):
                out.write(json.dumps({'expected': l, 'actual': r}, sort_keys=True) + '\n')
                count += 1
    if count:
        raise RuntimeError(f'{count} full-manifest differences; see {differences}')
