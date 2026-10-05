#!/usr/bin/env python3
"""Lease, durable handle, stream and lock use per phase (#535)."""
import os, sys, collections
sys.path.insert(0, os.path.dirname(__file__))
from analyze import load, band

def label(r):
    p = r.get("path", "")
    if band(p) is not None: return "bands/*"
    if "/mapped/" in p: return "mapped/*"
    return p.rsplit("/", 1)[-1] or "<root>"

def report(rows, title):
    creates = [r for r in rows if r["cmd"] == "CREATE"]
    print(f"\n## {title}: {len(rows)} requests")
    print("commands", dict(collections.Counter(r["cmd"] for r in rows).most_common()))
    print("CREATE contexts", dict(collections.Counter(r.get("ctx", "") for r in creates).most_common()))
    asked = [r for r in creates if "lease_req" in r]
    print("CREATE with RqLs", len(asked), "states asked", dict(collections.Counter(r["lease_req"] for r in asked)), "versions", dict(collections.Counter(r.get("lease_ver") for r in asked)))
    print("CREATE with DH2Q", sum("dh2q_timeout" in r for r in creates), "timeouts", dict(collections.Counter(r["dh2q_timeout"] for r in creates if "dh2q_timeout" in r)), "flags", dict(collections.Counter(r["dh2q_flags"] for r in creates if "dh2q_flags" in r)))
    print("reconnect CREATEs", sum(bool(r.get("reconnect")) for r in creates), "ok", sum(bool(r.get("reconnect")) and r["status"] == 0 for r in creates))
    ok = [r for r in creates if r["status"] == 0 and "lease_granted" in r]
    print("granted lease states", dict(collections.Counter(r["lease_granted"] for r in ok)), "durable granted", sum(r.get("durable", False) for r in ok))
    print("lease asked by file", dict(collections.Counter(label(r) for r in asked).most_common(12)))
    print("durable by file", dict(collections.Counter(label(r) for r in ok if r.get("durable")).most_common(12)))
    print("lease breaks sent", sum(r["cmd"] == "LEASE_BREAK_SENT" for r in rows), "acks", sum(r["cmd"] == "OPLOCK_BREAK" for r in rows))
    print("LOCK", dict(collections.Counter(f'{r["status"]:#x}' for r in rows if r["cmd"] == "LOCK")))
    print("stream CREATEs", dict(collections.Counter((label(r), r["stream"], f'{r["status"]:#x}') for r in creates if r.get("stream"))))
    print("fsattr", dict(collections.Counter(r.get("fsattr") for r in rows if "fsattr" in r)))
    print("failed", dict(collections.Counter((r["cmd"], f'{r["status"]:#x}') for r in rows if r["status"]).most_common(12)))

if __name__ == "__main__":
    import datetime
    log, *cuts = sys.argv[1:]
    rows = load([log])
    # cuts: name=HH:MM:SS pairs that start each phase
    bounds = [(c.split("=")[0], datetime.datetime.fromisoformat(rows[0]["time"][:11] + c.split("=")[1] + "+00:00")) for c in cuts]
    report(rows, "all")
    for i, (name, start) in enumerate(bounds):
        end = bounds[i + 1][1] if i + 1 < len(bounds) else rows[-1]["t"] + datetime.timedelta(seconds=1)
        report([r for r in rows if start <= r["t"] < end], name)
