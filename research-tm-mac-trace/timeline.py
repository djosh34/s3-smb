import sys, collections
sys.path.insert(0, __import__("os").path.dirname(__file__))
from analyze import load, band, cls
rows = load(sys.argv[1:])
# Per band, collapse runs of writes between non-write events.
cur = {}
def flush_run(b):
    r = cur.pop(b, None)
    if r: print(f"{b:x} {r['t0']} {r['t1']} W x{r['n']} bytes={r['bytes']} zero_writes={r['z']} off[{r['lo']},{r['hi']}) sizes={dict(r['sz'].most_common(3))}")
for r in rows:
    b = band(r["path"])
    if b is None: continue
    if r["cmd"] == "WRITE" and r["status"] == 0:
        c = cur.setdefault(b, {"t0": r["t"].strftime("%H:%M:%S.%f")[:-3], "n": 0, "bytes": 0, "z": 0, "lo": 1 << 62, "hi": 0, "sz": collections.Counter()})
        c["t1"] = r["t"].strftime("%H:%M:%S.%f")[:-3]; c["n"] += 1; c["bytes"] += r["len"]; c["z"] += r["zero"]; c["lo"] = min(c["lo"], r["off"]); c["hi"] = max(c["hi"], r["off"] + r["len"]); c["sz"][r["len"]] += 1
        continue
    if r["cmd"] in ("READ", "QUERY_INFO"): continue
    flush_run(b)
    extra = {k: r[k] for k in ("disp", "action", "size", "opts", "access", "eof", "flush", "delete", "class") if k in r}
    print(f"{b:x} {r['t'].strftime('%H:%M:%S.%f')[:-3]} {r['cmd']} st={r['status']:#x} fid={r.get('fid')} opens={r['opens']} {extra} {r['path'].split('/')[0][-30:]}")
for b in list(cur): flush_run(b)
