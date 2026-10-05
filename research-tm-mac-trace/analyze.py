#!/usr/bin/env python3
"""Analyse "smb trace" lines from the s3-smb application logs (#528)."""
import json, re, sys, collections, datetime

CHUNK = 8 << 20
QI_CLASS = {4: "Basic", 5: "Standard", 6: "Internal", 7: "Ea", 9: "Name", 15: "FullEa", 18: "All", 21: "AlternateName", 22: "Stream", 28: "NetworkOpen", 34: "AttributeTag", 35: "?35", 48: "NormalizedName"}
FS_CLASS = {1: "FsVolume", 3: "FsSize", 4: "FsDevice", 5: "FsAttribute", 7: "FsFullSize", 8: "FsObjectId", 11: "FsSectorSize"}
SI_CLASS = {4: "Basic", 10: "Rename", 13: "Disposition", 19: "Allocation", 20: "EndOfFile", 15: "FullEa", 64: "DispositionEx", 65: "RenameEx"}
QD_CLASS = {1: "Directory", 2: "Full", 3: "Both", 12: "Names", 37: "IdBoth", 38: "IdFull"}

def load(paths):
    rows = []
    for p in paths:
        for line in open(p, errors="replace"):
            if '"smb trace"' not in line:
                continue
            try:
                r = json.loads(line[line.index("{"):])
            except Exception:
                continue
            r["t"] = datetime.datetime.fromisoformat(r["time"].replace("Z", "+00:00"))
            r["path"] = r.get("path", "").replace("\\", "/")
            r["src"] = p
            rows.append(r)
    rows.sort(key=lambda r: r["t"])
    return rows

def cls(r):
    c, t, k = r["cmd"], r.get("type"), r.get("class")
    if c == "QUERY_DIRECTORY":
        return QD_CLASS.get(k, str(k))
    if t == 2:
        return FS_CLASS.get(k, f"fs{k}")
    if t == 3:
        return f"security"
    return (SI_CLASS if c == "SET_INFO" else QI_CLASS).get(k, str(k))

def band(path):
    m = re.search(r"/bands/([0-9a-f]+)$", path)
    return int(m.group(1), 16) if m else None

def kind(path):
    if band(path) is not None: return "band"
    base = path.rsplit("/", 1)[-1]
    return base

def main(paths):
    rows = load(paths)
    ok = [r for r in rows if r["status"] == 0]
    print(f"# trace lines {len(rows)}, first {rows[0]['t']} last {rows[-1]['t']}")
    print("\n## commands (all / success)")
    allc = collections.Counter(r["cmd"] for r in rows); okc = collections.Counter(r["cmd"] for r in ok)
    for c, n in allc.most_common(): print(f"{c:16} {n:8} {okc[c]:8}")
    print("\n## info classes (cmd type class: all / success)")
    ic = collections.Counter((r["cmd"], r.get("type"), cls(r), r["status"] == 0) for r in rows if r["cmd"] in ("QUERY_INFO", "SET_INFO", "QUERY_DIRECTORY"))
    keys = sorted({k[:3] for k in ic}, key=lambda k: (k[0], str(k[1]), k[2]))
    for k in keys: print(f"{k[0]:16} type={k[1]} {k[2]:14} {ic[k+(True,)]+ic[k+(False,)]:7} {ic[k+(True,)]:7}")
    print("\n## status codes by command (non-zero)")
    for (c, s), n in collections.Counter((r["cmd"], r["status"]) for r in rows if r["status"]).most_common(30): print(f"{c:16} {s:#010x} {n}")
    print("\n## IOCTL codes")
    for (k, s), n in collections.Counter((r.get("ctl"), r["status"]) for r in rows if r["cmd"] == "IOCTL").most_common(): print(f"{k:#x} status={s:#x} {n}")

    print("\n## streams")
    st = collections.defaultdict(lambda: collections.Counter())
    for r in rows:
        if r["cmd"] == "CREATE" and r.get("stream"):
            st[r["stream"]][("create", r["status"] == 0)] += 1
    for r in ok:
        if ":" in r["path"] and r["cmd"] in ("READ", "WRITE", "SET_INFO", "QUERY_INFO"):
            s = r["path"].split(":", 1)[1]
            if r["cmd"] == "WRITE": st[s]["write_bytes"] += r["len"]; st[s]["writes"] += 1; st[s]["max_end"] = max(st[s]["max_end"], r["off"] + r["len"])
            if r["cmd"] == "READ": st[s]["reads"] += 1; st[s]["read_req_bytes"] += r["len"]
            if r["cmd"] == "SET_INFO": st[s]["setinfo_" + cls(r)] += 1
            if r.get("eof") is not None: st[s]["eof_max"] = max(st[s]["eof_max"], r["eof"])
    for s, c in st.items(): print(s, dict(c))
    created_streams = collections.Counter((r["path"].rsplit("/", 1)[-1] if band(r["path"]) is None else "bands/*", r["stream"], r["status"]) for r in rows if r["cmd"] == "CREATE" and r.get("stream"))
    for k, n in created_streams.most_common(40): print("  create", k, n)
    print("stream sizes at open:", collections.Counter((r["stream"], r.get("size")) for r in ok if r["cmd"] == "CREATE" and r.get("stream")).most_common(20))

    print("\n## renames")
    for r in rows:
        if r["cmd"] == "SET_INFO" and r.get("target") is not None:
            print(r["t"].time(), f"status={r['status']:#x}", r["path"], "->", r["target"], "replace", r["replace"])
    print("\n## deletes (disposition / delete-on-close)")
    for r in rows:
        if r["cmd"] == "SET_INFO" and r.get("delete") is not None:
            print(r["t"].time(), f"status={r['status']:#x}", "delete", r["delete"], r["path"])
        if r["cmd"] == "CREATE" and r.get("opts", 0) & 0x1000:
            print(r["t"].time(), f"status={r['status']:#x}", "delete-on-close", r["path"], r.get("stream"))
    print("\n## EOF / allocation changes")
    eofs = [r for r in rows if r["cmd"] == "SET_INFO" and (r.get("eof") is not None or r.get("alloc") is not None)]
    byk = collections.Counter((kind(r["path"]), "eof" if r.get("eof") is not None else "alloc", r.get("eof", r.get("alloc")), r["status"]) for r in eofs)
    for k, n in byk.most_common(60): print(k, n)
    print("\n## destructive CREATE (supersede/overwrite) and actions")
    for k, n in collections.Counter((kind(r["path"]), r["disp"], r.get("action"), r["status"]) for r in rows if r["cmd"] == "CREATE").most_common(60): print(k, n)

    # Bands
    print("\n## band writes")
    bw = [r for r in ok if r["cmd"] == "WRITE" and band(r["path"]) is not None]
    print("writes", len(bw), "bytes", sum(r["len"] for r in bw), "zero writes", sum(r["zero"] for r in bw), "zero bytes", sum(r["len"] for r in bw if r["zero"]))
    print("sizes", collections.Counter(r["len"] for r in bw).most_common(15))
    print("offset alignment (off % 4096 == 0)", sum(r["off"] % 4096 == 0 for r in bw), "/", len(bw), " (off % 1MiB == 0)", sum(r["off"] % (1 << 20) == 0 for r in bw))
    seq = collections.Counter(); last = {}
    for r in bw:
        b = band(r["path"]); prev = last.get(b)
        seq["sequential" if prev == r["off"] else ("first" if prev is None else ("backward" if r["off"] < prev else "forward-gap"))] += 1
        last[b] = r["off"] + r["len"]
    print("order per band:", dict(seq))
    chunks = collections.defaultdict(list)
    for r in bw:
        b = band(r["path"]); o, e = r["off"], r["off"] + r["len"]
        while o < e:
            ci = o // CHUNK; ce = min(e, (ci + 1) * CHUNK)
            chunks[(b, ci)].append((o - ci * CHUNK, ce - o, r["zero"], r["t"]))
            o = ce
    def cover(iv):
        iv = sorted((a, a + l) for a, l, *_ in iv); runs = []; 
        for a, b in iv:
            if runs and a <= runs[-1][1]: runs[-1][1] = max(runs[-1][1], b)
            else: runs.append([a, b])
        return runs
    nwrites = []; nruns = []; full = 0; overw = 0
    for k, iv in chunks.items():
        runs = cover(iv); covered = sum(b - a for a, b in runs)
        nwrites.append(len(iv)); nruns.append(len(runs)); full += covered == CHUNK
        overw += sum(l for _, l, *_ in iv) > covered
    if chunks:
        print(f"chunks touched {len(chunks)}, fully covered {full}, with rewrites {overw}")
        print("writes per chunk", sorted(collections.Counter(nwrites).items())[:30])
        print("disjoint extents per chunk", sorted(collections.Counter(nruns).items())[:30])
    # Per band positions.
    eof = {}
    for r in rows:
        if band(r["path"]) is not None:
            if r["cmd"] == "SET_INFO" and r.get("eof") is not None and r["status"] == 0: eof[band(r["path"])] = max(eof.get(band(r["path"]), 0), r["eof"])
            if r["cmd"] == "CREATE" and r.get("size") is not None: eof[band(r["path"])] = max(eof.get(band(r["path"]), 0), r["size"])
    bands = sorted({band(r["path"]) for r in rows if band(r["path"]) is not None})
    print("\nband  maxend  eof  positions  never  zero-only  partial-zero  mixed")
    for b in bands:
        maxend = max([r["off"] + r["len"] for r in bw if band(r["path"]) == b] or [0])
        size = max(maxend, eof.get(b, 0)); npos = -(-size // CHUNK)
        never = zero = mixed = 0
        for ci in range(npos):
            iv = chunks.get((b, ci))
            if not iv: never += 1; continue
            if all(z for _, _, z, _ in iv): zero += 1
            elif any(z for _, _, z, _ in iv): mixed += 1
        print(f"{b:x} {maxend} {eof.get(b)} {npos} {never} {zero} {mixed}")

    print("\n## FLUSH per file")
    fl = [r for r in ok if r["cmd"] == "FLUSH"]
    print("flushes", len(fl), collections.Counter(r["flush"] for r in fl))
    per = collections.defaultdict(list)
    for r in fl: per[r["path"]].append(r)
    dirty = collections.Counter(); maxdirty = collections.Counter(); since = {}
    for r in ok:
        if r["cmd"] == "WRITE": dirty[r["path"]] += r["len"]
        if r["cmd"] == "FLUSH":
            maxdirty[r["path"]] = max(maxdirty[r["path"]], dirty[r["path"]]); dirty[r["path"]] = 0
    for p, n in dirty.items(): maxdirty[p] = max(maxdirty[p], n)  # unflushed tail
    kinds = collections.defaultdict(lambda: collections.Counter())
    for p, fs in per.items(): kinds[kind(p)]["files"] += 1; kinds[kind(p)]["flushes"] += len(fs); kinds[kind(p)]["full"] += sum(f["flush"] == "full" for f in fs)
    for k, c in sorted(kinds.items(), key=lambda kv: -kv[1]["flushes"])[:30]: print(k, dict(c))
    print("most bytes between flushes:", [(p, n) for p, n in maxdirty.most_common(10)])
    gaps = []
    for p, fs in per.items():
        for a, b in zip(fs, fs[1:]): gaps.append((b["t"] - a["t"]).total_seconds())
    if gaps:
        gaps.sort(); print("gap between flushes of one file: n", len(gaps), "median", gaps[len(gaps)//2], "p90", gaps[int(len(gaps)*.9)], "max", gaps[-1])
    print("flush latency us median/max:", sorted(r["us"] for r in fl)[len(fl)//2] if fl else None, max((r["us"] for r in fl), default=None))
    # flushes on bands per minute
    bm = collections.Counter(r["t"].strftime("%H:%M") for r in fl if band(r["path"]) is not None)
    print("band flushes per minute", sorted(bm.items()))

    print("\n## handles")
    po = max(rows, key=lambda r: r["opens"]); pf = max(rows, key=lambda r: r["files"])
    print("peak opens", po["opens"], po["t"], "peak distinct files", pf["files"], pf["t"])
    print("open-band peaks: max concurrent band fids", end=" ")
    openb = set(); peak = 0
    for r in rows:
        if r["cmd"] == "CREATE" and r["status"] == 0 and band(r["path"]) is not None: openb.add(r["fid"])
        if r["cmd"] == "CLOSE" and r["status"] == 0: openb.discard(r.get("fid"))
        peak = max(peak, len(openb))
    print(peak)

    print("\n## heads")
    for r in ok:
        if r["cmd"] == "WRITE" and r.get("head") is not None and (kind(r["path"]) in ("token", "Info.plist") or band(r["path"]) == 0):
            print(r["t"].time(), r["path"], r["len"], r["head"][:64], r.get("at512"))
    for r in ok:
        if r["cmd"] == "CREATE" and r["path"].endswith("/token"):
            print("token open", r["t"].time(), "disp", r["disp"], "action", r.get("action"), "size", r.get("size"))

if __name__ == "__main__":
    main(sys.argv[1:])
