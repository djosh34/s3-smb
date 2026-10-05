#!/usr/bin/env python3
"""Bytes waiting in a local scratch folder at once, at 8 MiB chunks (#598).

Usage:
  python3 scratch.py application-1-initialize.log mac-harness.log

Model: a WRITE makes each chunk it touches dirty. A dirty chunk sits in
scratch as one file of min(8 MiB, file size - chunk start) bytes, until a FLUSH
or CLOSE of its file uploads it, or the file is deleted or truncated. Uploads
are taken as instant.

"Sparse" counts a dirty chunk as 0 bytes while it holds only zeros: every
write into it was all zeros, and it had no earlier nonzero version.
"""
import json
import sys
from collections import defaultdict

from hotspots import norm, phase_of, ts, windows_from

CHUNK = 8 << 20
MiB = 1 << 20


def main():
    log, harness = sys.argv[1], sys.argv[2]
    wins = windows_from([harness])
    fsize = defaultdict(int)
    dirty = defaultdict(set)  # path -> chunks
    nonzero = defaultdict(set)  # path -> chunks with nonzero data, ever
    pending_delete = {}
    full = defaultdict(int)  # path -> scratch bytes
    sparse = defaultdict(int)
    tot = {"full": 0, "sparse": 0}
    samples = []  # (time, full, sparse, path)

    def recount(path):
        size = fsize[path]
        f = s = 0
        for c in dirty.get(path, ()):
            b = max(0, min(CHUNK, size - c * CHUNK))
            f += b
            s += b if c in nonzero[path] else 0
        tot["full"] += f - full[path]
        tot["sparse"] += s - sparse[path]
        full[path], sparse[path] = f, s

    def clear(path, keep_below=0):
        first = -(-keep_below // CHUNK)
        dirty[path] = {c for c in dirty.get(path, set()) if c < first}
        if not keep_below:
            nonzero.pop(path, None)

    for line in open(log, errors="replace"):
        if '"smb trace"' not in line:
            continue
        r = json.loads(line)
        if r.get("status") != 0:
            continue
        t, cmd, path = ts(r["time"]), r["cmd"], norm(r.get("path", ""))
        if cmd == "CREATE":
            if r.get("action") in (0, 3):
                clear(path)
                fsize[path] = 0
            elif r.get("action") == 2:
                fsize[path] = r.get("size", 0)
                nonzero.pop(path, None)
            else:
                fsize[path] = max(fsize[path], r.get("size", 0))
        elif cmd == "WRITE":
            off, n = r["off"], r["len"]
            if n > 0:
                for c in range(off // CHUNK, (off + n - 1) // CHUNK + 1):
                    dirty[path].add(c)
                    if not r.get("zero", False):
                        nonzero[path].add(c)
                fsize[path] = max(fsize[path], off + n)
        elif cmd in ("FLUSH", "CLOSE"):
            dirty.pop(path, None)
            if cmd == "CLOSE" and pending_delete.pop(r["fid"], False):
                clear(path)
                fsize.pop(path, None)
        elif cmd == "SET_INFO":
            if "delete" in r:
                pending_delete[r["fid"]] = r["delete"]
            elif "eof" in r:
                if r["eof"] < fsize[path]:
                    clear(path, r["eof"])
                fsize[path] = r["eof"]
            elif "target" in r:
                dst = norm(r["target"])
                if dst != path:
                    for m in (dirty, nonzero, fsize):
                        if path in m:
                            m[dst] = m.pop(path)
                    full[dst], sparse[dst] = full.pop(path, 0), sparse.pop(path, 0)
                    recount(dst)
                    path = dst
        recount(path)
        samples.append((t, tot["full"], tot["sparse"], path))

    def stats(rows, i):
        if not rows:
            return None
        peak = max(rows, key=lambda x: x[i])
        # p99 weighted by time: the value held for 99% of the time.
        held = []
        for a, b in zip(rows, rows[1:]):
            held.append((a[i], b[0] - a[0]))
        held.sort()
        total = sum(d for v, d in held) or 1
        acc, p99 = 0, 0
        for v, d in held:
            acc += d
            if acc >= 0.99 * total:
                p99 = v
                break
        ev = sorted(x[i] for x in rows)
        return {"peak_mib": round(peak[i] / MiB, 1), "peak_time": peak[0], "peak_path": peak[3], "p99_time_mib": round(p99 / MiB, 1), "p99_requests_mib": round(ev[int(0.99 * (len(ev) - 1))] / MiB, 1)}

    out = {}
    for name, i in (("full", 1), ("sparse", 2)):
        out[name] = {"whole run": stats(samples, i)}
        for label in [w[0] for w in wins] + ["other"]:
            out[name][label] = stats([x for x in samples if phase_of(x[0], wins) == label], i)
    from datetime import datetime, timezone
    for name in out:
        print(f"\n{name}: peak MiB, p99 over time MiB, p99 over requests MiB, when, file")
        for label, s in out[name].items():
            if s:
                when = datetime.fromtimestamp(s["peak_time"], timezone.utc).strftime("%H:%M:%S")
                print(f"  {label:10} {s['peak_mib']:8} {s['p99_time_mib']:8} {s['p99_requests_mib']:8}  {when} {s['peak_path']}")


if __name__ == "__main__":
    main()
