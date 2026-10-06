#!/usr/bin/env python3
"""Chunk versions, hot spots, trash and database rows from an s3-smb trace (#598).

Usage:
  python3 hotspots.py application-1-initialize.log mac-harness.log [out.json]
  python3 hotspots.py application-1-initialize.log label=START,END ... [out.json]

The harness log gives the backup windows ("hotspots-backup-start" and
"hotspots-backup-end" lines). Without it, pass label=start,end pairs in UTC
ISO time.

Model (the design in #592):
- A WRITE marks the chunks it touches dirty in its file.
- A FLUSH of a file uploads every dirty chunk of that file as a new version.
  A CLOSE with dirty chunks and no FLUSH does the same (counted apart).
- A chunk object is min(chunk size, file size - chunk start) bytes.
- The version it replaces goes to the trash at that moment.
- A delete, a truncate or an overwriting CREATE sends the dropped chunks to the
  trash. A rename moves the chunks to the new name.
Paths inside the sparsebundle are taken relative to the bundle, so the
".incomplete" to ".sparsebundle" rename does not matter.
"""
import json
import re
import sys
from collections import defaultdict
from datetime import datetime, timezone

SIZES = {"8M": 8 << 20, "1M": 1 << 20, "256K": 256 << 10, "4K": 4 << 10}
COPY = 15 * 60
KEEP = 7 * 86400


def ts(s):
    s = s.rstrip("Z")
    if "." in s:
        a, b = s.split(".")
        s = a + "." + b[:6].ljust(6, "0")
    return datetime.fromisoformat(s).replace(tzinfo=timezone.utc).timestamp()


def norm(path):
    parts = path.split("\\")
    if len(parts) > 1 and (parts[0].endswith(".sparsebundle") or parts[0].endswith(".incomplete")):
        return "/".join(parts[1:])
    if parts[0].endswith(".sparsebundle") or parts[0].endswith(".incomplete"):
        return "<bundle>"
    return "/".join(parts)


def windows_from(args):
    wins = []
    harness = [a for a in args if not "=" in a and not a.endswith(".json")]
    for a in args:
        if "=" in a:
            label, span = a.split("=", 1)
            s, e = span.split(",")
            wins.append((label, ts(s), ts(e)))
    for path in harness:
        starts, ends = {}, {}
        for line in open(path, errors="replace"):
            m = re.search(r"hotspots-backup-(start|end) (\S+) (\S+)", line)
            if m:
                (starts if m.group(1) == "start" else ends)[m.group(2)] = ts(m.group(3))
        for label in starts:
            if label in ends:
                wins.append((label, starts[label], ends[label]))
    wins.sort(key=lambda w: w[1])
    return wins


class Store:
    """Chunk versions of every file at one chunk size."""

    def __init__(self, size):
        self.size = size
        self.live = defaultdict(dict)  # path -> chunk -> (written, bytes)
        self.dirty = defaultdict(set)
        self.versions = []  # (path, chunk, written, replaced or None, bytes, cause)

    def write(self, path, off, length):
        if length <= 0:
            return
        for c in range(off // self.size, (off + length - 1) // self.size + 1):
            self.dirty[path].add(c)

    def commit(self, path, t, fsize, cause):
        n = 0
        for c in sorted(self.dirty.pop(path, ())):
            start = c * self.size
            if start >= fsize:
                continue
            b = min(self.size, fsize - start)
            old = self.live[path].get(c)
            if old is not None:
                self.versions[old][3] = t
                self.versions[old].append("rewrite")
            self.versions.append([path, c, t, None, b, cause])
            self.live[path][c] = len(self.versions) - 1
            n += 1
        return n

    def drop(self, path, t, keep_below=0):
        """Trash every live chunk of path at or past byte keep_below."""
        first = -(-keep_below // self.size)
        for c in [c for c in self.live.get(path, {}) if c >= first]:
            i = self.live[path].pop(c)
            self.versions[i][3] = t
            self.versions[i].append("drop")
        if keep_below:
            # A partial last chunk is rewritten by the next commit.
            self.dirty[path] = {c for c in self.dirty.get(path, set()) if c < first}
        else:
            self.dirty.pop(path, None)
        if not self.live.get(path):
            self.live.pop(path, None)

    def move(self, src, dst, t):
        self.drop(dst, t)
        if src in self.live:
            self.live[dst] = self.live.pop(src)
            for i in self.live[dst].values():
                self.versions[i][0] = dst
        if src in self.dirty:
            self.dirty[dst] = self.dirty.pop(src)


def run(log, wins):
    stores = {k: Store(v) for k, v in SIZES.items()}
    fsize = defaultdict(int)
    pending_delete = {}
    fids = {}
    jfs = []  # (time, path, bytes written, unique bytes) per FLUSH
    jfs_ranges = defaultdict(list)
    commits = defaultdict(int)
    writes = []  # (time, path, off, len, zero)
    first = last = None
    for line in open(log, errors="replace"):
        if '"smb trace"' not in line:
            continue
        r = json.loads(line)
        t = ts(r["time"])
        first = t if first is None else first
        last = t
        cmd, ok = r["cmd"], r.get("status") == 0
        if not ok:
            continue
        path = norm(r.get("path", ""))
        if cmd == "CREATE":
            fids[r["fid"]] = path
            if r.get("action") in (0, 3):  # superseded or overwritten
                for s in stores.values():
                    s.drop(path, t)
                fsize[path] = 0
                jfs_ranges.pop(path, None)
            elif r.get("action") == 2:  # created
                fsize[path] = r.get("size", 0)
            else:
                fsize[path] = max(fsize[path], r.get("size", 0))
        elif cmd == "WRITE":
            off, n = r["off"], r["len"]
            for s in stores.values():
                s.write(path, off, n)
            fsize[path] = max(fsize[path], off + n)
            jfs_ranges[path].append((off, off + n))
            writes.append((t, path, off, n, r.get("zero", False)))
        elif cmd == "FLUSH" or cmd == "CLOSE":
            cause = "flush" if cmd == "FLUSH" else "close"
            got = [s.commit(path, t, fsize[path], cause) for s in stores.values()]
            if any(got):
                commits[cause] += 1
            rng = jfs_ranges.pop(path, [])
            if rng:
                total = sum(e - b for b, e in rng)
                rng.sort()
                uniq, cur_b, cur_e = 0, None, None
                for b, e in rng:
                    if cur_e is None or b > cur_e:
                        if cur_e is not None:
                            uniq += cur_e - cur_b
                        cur_b, cur_e = b, e
                    else:
                        cur_e = max(cur_e, e)
                uniq += cur_e - cur_b
                jfs.append((t, path, total, uniq))
            if cmd == "CLOSE" and pending_delete.pop(r["fid"], False):
                for s in stores.values():
                    s.drop(path, t)
                fsize.pop(path, None)
        elif cmd == "SET_INFO":
            if "delete" in r:
                pending_delete[r["fid"]] = r["delete"]
            elif "eof" in r:
                eof = r["eof"]
                if eof < fsize[path]:
                    for s in stores.values():
                        s.drop(path, t, eof)
                fsize[path] = eof
            elif "target" in r:
                dst = norm(r["target"])
                if dst != path:
                    for s in stores.values():
                        s.move(path, dst, t)
                    fsize[dst] = fsize.pop(path, 0)
                    if path in jfs_ranges:
                        jfs_ranges[dst] = jfs_ranges.pop(path)
    return stores, fsize, jfs, commits, writes, first, last


def phase_of(t, wins):
    for label, s, e in wins:
        if s <= t <= e:
            return label
    return "other"


def retime(wins):
    """Map trace time to a timeline with one backup per hour (computed).

    Backup i starts an hour after backup i-1, or a minute after it ended if
    it ran longer than an hour.
    """
    starts = []
    for i, (label, s, e) in enumerate(wins):
        if i == 0:
            starts.append(0.0)
        else:
            ps, pe = wins[i - 1][1], wins[i - 1][2]
            starts.append(max(starts[-1] + 3600, starts[-1] + (pe - ps) + 60))

    def f(t):
        for i, (label, s, e) in enumerate(wins):
            nxt = wins[i + 1][1] if i + 1 < len(wins) else float("inf")
            if t < s and i == 0:
                return t - s
            if s <= t < nxt:
                if t <= e:
                    return starts[i] + (t - s)
                room = (starts[i + 1] if i + 1 < len(wins) else starts[i] + 3600) - starts[i] - (e - s) - 1
                return starts[i] + (e - s) + min(t - e, room)
        return t - wins[0][1]
    return f


def pinned(w, r, offset):
    """True if a copy at offset + k*COPY falls in [w, r)."""
    k = -(-(w - offset) // COPY)
    return offset + k * COPY < r


def short_wait_delete(r, offset):
    """Owner's rule from #597: deleted when the 5th copy after r has landed."""
    k = (r - offset) // COPY + 1
    return offset + (k + 4) * COPY


def peak(events):
    """Largest sum held, from (time, +bytes or -bytes) events."""
    held = top = 0
    for t, b in sorted(events, key=lambda e: (e[0], e[1])):
        held += b
        top = max(top, held)
    return top


def analyse(stores, fsize, jfs, commits, writes, wins, first, last):
    labels = [w[0] for w in wins] + ["other"]
    out = {"windows": [{"label": l, "start": datetime.fromtimestamp(s, timezone.utc).isoformat(), "end": datetime.fromtimestamp(e, timezone.utc).isoformat(), "seconds": round(e - s, 1)} for l, s, e in wins]}
    out["commits"] = dict(commits)
    # Writes per backup (measured).
    wr = {l: {"writes": 0, "bytes": 0, "zero_bytes": 0, "band_bytes": 0} for l in labels}
    for t, p, off, n, z in writes:
        d = wr[phase_of(t, wins)]
        d["writes"] += 1
        d["bytes"] += n
        d["zero_bytes"] += n if z else 0
        d["band_bytes"] += n if p.startswith("bands/") else 0
    out["writes"] = wr
    # Versions uploaded and trash per backup (computed).
    hourly = retime(wins)
    offsets = [m * 60 for m in range(15)]
    up, trash = {}, {}
    hotset = {}
    for k, s in stores.items():
        hotset[k] = defaultdict(int)
        for p, c, w, r, b, cause, *end in s.versions:
            hotset[k][(p, c, phase_of(w, wins))] += 1
    for k, s in stores.items():
        up[k] = {l: {"versions": 0, "bytes": 0, "band_versions": 0, "band_bytes": 0} for l in labels}
        trash[k] = {l: {"A_versions": 0, "A_bytes": 0, "B_ci_bytes": [0] * 15, "B_hourly_bytes": [0] * 15, "B_ci_versions": [0] * 15, "B_hourly_versions": [0] * 15} for l in labels}
        for p, c, w, r, b, cause, *end in s.versions:
            ph = phase_of(w, wins)
            u = up[k][ph]
            u["versions"] += 1
            u["bytes"] += b
            if p.startswith("bands/"):
                u["band_versions"] += 1
                u["band_bytes"] += b
            if r is None:
                continue
            d = trash[k][phase_of(r, wins)]
            d["A_versions"] += 1
            d["A_bytes"] += b
            d["A_" + end[0] + "_bytes"] = d.get("A_" + end[0] + "_bytes", 0) + b
            if hotset[k].get((p, c, ph), 0) >= 10:
                d["A_hot_bytes"] = d.get("A_hot_bytes", 0) + b
            hw, hr = hourly(w), hourly(r)
            for i, o in enumerate(offsets):
                if pinned(w, r, wins[0][1] + o):
                    d["B_ci_bytes"][i] += b
                    d["B_ci_versions"][i] += 1
                if pinned(hw, hr, o):
                    d["B_hourly_bytes"][i] += b
                    d["B_hourly_versions"][i] += 1
    out["uploads"] = up
    for k in trash:
        for l in trash[k]:
            d = trash[k][l]
            for key in ("B_ci_bytes", "B_hourly_bytes", "B_ci_versions", "B_hourly_versions"):
                v = d[key]
                d[key] = {"mean": sum(v) / len(v), "min": min(v), "max": max(v)}
    out["trash"] = trash
    # Short wait (#597): every replaced version is held until 5 newer copies
    # have landed, 60 to 75 minutes. Peak bytes held at once, mean over the
    # copy-grid offsets. Harness-phase replacements are left out.
    short = {}
    inc_start = next((s for l, s, e in wins if l.startswith("inc")), None)
    for k, st in stores.items():
        res = {}
        for name, f, base in (("ci", lambda t: t, wins[0][1]), ("hourly", hourly, 0)):
            peaks, peaks_inc, per = [], [], {l: [] for l in labels}
            for o in offsets:
                ev, ev_inc = [], []
                held = defaultdict(float)
                for p, c, w, r, b, cause, *end in st.versions:
                    if r is None or phase_of(r, wins) == "other":
                        continue
                    fr = f(r)
                    d = short_wait_delete(fr, base + o)
                    ev += [(fr, b), (d, -b)]
                    if inc_start is not None and r >= inc_start:
                        ev_inc += [(fr, b), (d, -b)]
                peaks.append(peak(ev))
                peaks_inc.append(peak(ev_inc) if ev_inc else 0)
            res[name + "_peak"] = {"mean": sum(peaks) / len(peaks), "min": min(peaks), "max": max(peaks)}
            res[name + "_peak_incrementals"] = {"mean": sum(peaks_inc) / len(peaks_inc), "min": min(peaks_inc), "max": max(peaks_inc)}
        short[k] = res
    out["short_wait"] = short
    # JuiceFS: bytes written between FLUSHes (computed).
    j = {l: {"written": 0, "unique": 0} for l in labels}
    for t, p, tot, uniq in jfs:
        j[phase_of(t, wins)]["written"] += tot
        j[phase_of(t, wins)]["unique"] += uniq
    out["juicefs"] = j
    # Rows at the end (computed).
    rows = {}
    for k, s in stores.items():
        live = sum(len(v) for v in s.live.values())
        full = sum(-(-sz // s.size) for p, sz in fsize.items() if sz > 0)
        lb = sum(b for p, v in s.live.items() for c, i in v.items() for b in [s.versions[i][4]])
        rows[k] = {"live_rows": live, "rows_if_every_chunk_in_eof": full, "live_bytes": lb, "files": len([p for p, sz in fsize.items() if sz > 0])}
    out["rows"] = rows
    # Hot spots: versions per chunk per backup (computed).
    hot = {}
    for k in ("8M", "256K", "4K"):
        per = defaultdict(lambda: defaultdict(int))
        for p, c, w, r, b, cause, *end in stores[k].versions:
            per[(p, c)][phase_of(w, wins)] += 1
        ranked = sorted(per.items(), key=lambda kv: -sum(v for l, v in kv[1].items() if l != "other"))
        hot[k] = [{"file": p, "chunk": c, "offset": c * SIZES[k], "per_backup": {l: v.get(l, 0) for l in labels}} for (p, c), v in ranked[:25]]
        # Same spots each backup: top 10 per backup.
        tops = {}
        for l in labels[:-1]:
            items = sorted(((v.get(l, 0), pc) for pc, v in per.items() if v.get(l, 0) > 0), reverse=True)[:10]
            tops[l] = [f"{p}@{c}" for n, (p, c) in items]
        hot[k + "_top10"] = tops
        # Chunks with 10 or more versions in a backup.
        hot[k + "_hot_count"] = {l: sum(1 for v in per.values() if v.get(l, 0) >= 10) for l in labels}
        hot[k + "_versions_in_hot"] = {l: sum(v.get(l, 0) for v in per.values() if v.get(l, 0) >= 10) for l in labels}
    out["hot"] = hot
    return out


def main():
    args = sys.argv[1:]
    log = args[0]
    outp = next((a for a in args[1:] if a.endswith(".json")), None)
    wins = windows_from(args[1:])
    if not wins:
        sys.exit("no backup windows")
    stores, fsize, jfs, commits, writes, first, last = run(log, wins)
    res = analyse(stores, fsize, jfs, commits, writes, wins, first, last)
    text = json.dumps(res, indent=1)
    if outp:
        open(outp, "w").write(text)
    print(text)


if __name__ == "__main__":
    main()
