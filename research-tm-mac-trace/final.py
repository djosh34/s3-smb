import sys, collections
sys.path.insert(0, __import__("os").path.dirname(__file__))
from analyze import load, band
CH = 8 << 20
rows = load(sys.argv[1:])
inc = collections.defaultdict(list)  # band -> list of incarnations (list of writes)
size = {}
for r in rows:
    b = band(r["path"])
    if b is None or r["status"]: continue
    if r["cmd"] == "CREATE" and r.get("action") == 2: inc[b].append([])
    if r["cmd"] == "WRITE": inc[b][-1].append(r)
    if r["cmd"] == "CREATE" and r.get("size") is not None: size[b] = r["size"]
    if r["cmd"] == "SET_INFO" and r.get("eof") is not None: size[b] = r["eof"]
deleted = collections.Counter(band(r["path"]) for r in rows if r["cmd"] == "SET_INFO" and r.get("delete") and band(r["path"]) is not None and r["status"] == 0)
print("band incarnations", {f"{b:x}": len(v) for b, v in inc.items()}, "deleted", {f"{b:x}": n for b, n in deleted.items()})
print("\nfinal incarnation per band: size, positions, never, zero-only, nonzero-only, mixed, writes, zero writes, nonzero bytes")
for b, v in sorted(inc.items()):
    ws = v[-1]; alive = len(v) > deleted[b]
    end = max([w["off"] + w["len"] for w in ws] or [0])
    sz = max(end, size.get(b, 0)) if alive else end
    pos = collections.defaultdict(lambda: [0, 0])
    for w in ws:
        o, e = w["off"], w["off"] + w["len"]
        for ci in range(o // CH, (e - 1) // CH + 1): pos[ci][0 if w["zero"] else 1] += 1
    npos = -(-sz // CH)
    never = sum(1 for i in range(npos) if i not in pos)
    zo = sum(1 for p in pos.values() if p[1] == 0); nz = sum(1 for p in pos.values() if p[0] == 0); mx = len(pos) - zo - nz
    print(f"{b:x} {'alive' if alive else 'deleted'} size={sz} positions={npos} never={never} zero_only={zo} nonzero_only={nz} mixed={mx} writes={len(ws)} zero_writes={sum(w['zero'] for w in ws)} nonzero_bytes={sum(w['len'] for w in ws if not w['zero'])}")
    print("   per-position nonzero write counts:", {i: p[1] for i, p in sorted(pos.items()) if p[1]})
# Scatter of nonzero writes within 8 MiB chunks.
nz = [r for r in rows if r["cmd"] == "WRITE" and r["status"] == 0 and band(r["path"]) is not None and not r["zero"]]
print("\nnonzero band writes", len(nz), "bytes", sum(r["len"] for r in nz), "sizes", collections.Counter(r["len"] for r in nz).most_common(10))
chunks = collections.defaultdict(list)
for r in nz: chunks[(band(r["path"]), r["off"] // CH)].append(r)
for k, ws in sorted(chunks.items()):
    offs = [w["off"] % CH for w in ws]
    seq = sum(1 for a, c in zip(ws, ws[1:]) if c["off"] == a["off"] + a["len"])
    back = sum(1 for a, c in zip(ws, ws[1:]) if c["off"] < a["off"])
    covered = set()
    for w in ws:
        for blk in range(w["off"] // 4096, (w["off"] + w["len"]) // 4096): covered.add(blk)
    rewrites = sum(w["len"] // 4096 for w in ws) - len(covered)
    print(f"band {k[0]:x} chunk {k[1]}: writes={len(ws)} bytes={sum(w['len'] for w in ws)} distinct4k={len(covered)} rewritten4k={rewrites} span=[{min(offs)},{max(o+w['len'] for o, w in zip(offs, ws))}) sequential_next={seq} backward_next={back} time={ws[0]['t'].strftime('%S.%f')[:6]}..{ws[-1]['t'].strftime('%M:%S')}")
