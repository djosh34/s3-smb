import sys, collections, datetime, re
sys.path.insert(0, __import__("os").path.dirname(__file__))
from analyze import load, band, cls, kind
rows = load(sys.argv[1:])
T = lambda s: datetime.datetime.fromisoformat("2026-10-05T" + s + "+00:00")
phases = [("setdestination", T("09:28:00"), T("09:28:19")), ("backup", T("09:28:19"), T("09:29:13")), ("verify", T("09:29:13"), T("09:31:00"))]
for name, a, b in phases:
    ph = [r for r in rows if a <= r["t"] < b]
    c = collections.Counter(r["cmd"] for r in ph)
    print(name, len(ph), dict(c.most_common()))
    print("   classes", dict(collections.Counter((r["cmd"][:2] + str(r.get("type", "")), cls(r)) for r in ph if r["cmd"] in ("QUERY_INFO", "SET_INFO", "QUERY_DIRECTORY")).most_common()))
    print("   write bytes", sum(r["len"] for r in ph if r["cmd"] == "WRITE"), "read bytes req", sum(r["len"] for r in ph if r["cmd"] == "READ"), "flush", collections.Counter(r["flush"] for r in ph if r["cmd"] == "FLUSH"))
    print("   peak opens", max((r["opens"] for r in ph), default=0), "peak files", max((r["files"] for r in ph), default=0))
print("\nfailed CREATEs")
for k, n in collections.Counter((kind(r["path"]) if band(r["path"]) is None else "bands/<hex>", r.get("stream"), f'{r["status"]:#x}') for r in rows if r["cmd"] == "CREATE" and r["status"]).most_common(): print(" ", k, n)
print("Stream-info queries on", collections.Counter(kind(r["path"]) or "<root>" for r in rows if r["cmd"] == "QUERY_INFO" and r.get("class") == 22 and r.get("type") == 1))
print("security queries on", collections.Counter(kind(r["path"]) or "<root>" for r in rows if r["cmd"] == "QUERY_INFO" and r.get("type") == 3))
print("QUERY_DIRECTORY", collections.Counter((kind(r["path"]) or "<root>", r["pattern"][:40], f'{r["status"]:#x}') for r in rows if r["cmd"] == "QUERY_DIRECTORY").most_common(40))
print("AFP_AfpInfo times", [str(r["t"].time()) for r in rows if r.get("stream") == "AFP_AfpInfo"])
print("quarantine times", [str(r["t"].time()) for r in rows if r.get("stream") == "com.apple.quarantine"])
print("reads by file", collections.Counter(kind(r["path"]) if band(r["path"]) is None else "band" for r in rows if r["cmd"] == "READ").most_common(15))
print("read sizes", collections.Counter(r["len"] for r in rows if r["cmd"] == "READ").most_common(8))
print("writes by file kind", collections.Counter(kind(r["path"]) if band(r["path"]) is None else "band" for r in rows if r["cmd"] == "WRITE").most_common(20))
print("mapped writes", [(r["path"].split('/')[-1], r["off"], r["len"], r["zero"]) for r in rows if r["cmd"] == "WRITE" and "/mapped/" in r["path"]][:20])
print("token writes", [(r["off"], r["len"]) for r in rows if r["cmd"] == "WRITE" and r["path"].endswith("/token")])
print("write-through flags", collections.Counter(r["wflags"] for r in rows if r["cmd"] == "WRITE"), "create opts", collections.Counter(r["opts"] for r in rows if r["cmd"] == "CREATE"))
print("lease/durable n/a; tree connects", collections.Counter(f'{r["status"]:#x}' for r in rows if r["cmd"] == "TREE_CONNECT"))
