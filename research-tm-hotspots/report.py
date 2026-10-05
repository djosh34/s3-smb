#!/usr/bin/env python3
"""Markdown tables from the JSON that hotspots.py writes (#598).

Usage: python3 report.py out.json [incremental labels to average, default inc*]
"""
import json
import sys

d = json.load(open(sys.argv[1]))
labels = [w["label"] for w in d["windows"]]
incs = sys.argv[2].split(",") if len(sys.argv) > 2 else [l for l in labels if l.startswith("inc")]
SIZES = ["8M", "1M", "256K"]
MiB = 1 << 20
GiB = 1 << 30


def mib(b):
    return f"{b / MiB:,.0f}" if b >= 10 * MiB else f"{b / MiB:,.1f}"


def gib(b):
    return f"{b / GiB:,.1f}"


print("## Backups\n")
print("| Backup | Seconds | WRITEs | Bytes written (MiB) | of which zeros | JuiceFS upload (MiB) |")
print("|---|---:|---:|---:|---:|---:|")
for w in d["windows"]:
    l = w["label"]
    x = d["writes"][l]
    print(f"| {l} | {w['seconds']:.0f} | {x['writes']:,} | {mib(x['bytes'])} | {mib(x['zero_bytes'])} | {mib(d['juicefs'][l]['written'])} |")
x = d["writes"]["other"]
print(f"| harness, between backups | | {x['writes']:,} | {mib(x['bytes'])} | {mib(x['zero_bytes'])} | {mib(d['juicefs']['other']['written'])} |")

print("\n## Chunk versions uploaded\n")
print("| Backup | " + " | ".join(f"{s} versions | {s} MiB" for s in SIZES) + " |")
print("|---|" + "---:|" * (2 * len(SIZES)))
for l in labels + ["other"]:
    row = []
    for s in SIZES:
        u = d["uploads"][s][l]
        row += [f"{u['versions']:,}", mib(u["bytes"])]
    print(f"| {l} | " + " | ".join(row) + " |")
tot = []
for s in SIZES:
    tot += [f"{sum(d['uploads'][s][l]['versions'] for l in labels + ['other']):,}", mib(sum(d["uploads"][s][l]["bytes"] for l in labels + ["other"]))]
print("| total | " + " | ".join(tot) + " |")

print("\n## Trash per backup (MiB)\n")
print("A: every replaced version kept 7 days. B-lite: replaced versions that no 15-minute copy saw are deleted at once. B-lite is the mean over 15 copy-grid offsets, one per minute, with min and max.\n")
for mode, title in (("B_ci_bytes", "as run, backups back to back"), ("B_hourly_bytes", "same events, one backup per hour")):
    print(f"\n### B-lite timeline: {title}\n")
    print("| Backup | " + " | ".join(f"{s} A | {s} B-lite (min to max)" for s in SIZES) + " |")
    print("|---|" + "---:|" * (2 * len(SIZES)))
    for l in labels + ["other"]:
        row = []
        for s in SIZES:
            t = d["trash"][s][l]
            b = t[mode]
            row += [mib(t["A_bytes"]), f"{mib(b['mean'])} ({mib(b['min'])} to {mib(b['max'])})"]
        print(f"| {l} | " + " | ".join(row) + " |")

print("\n## Where trash comes from (MiB, A)\n")
print("Hot: the replaced version belongs to a chunk with 10 or more versions in that backup. Dropped: deleted or truncated files.\n")
print("| Backup | " + " | ".join(f"{s} sent | {s} hot | {s} dropped" for s in SIZES) + " |")
print("|---|" + "---:|" * (3 * len(SIZES)))
for l in labels + ["other"]:
    row = []
    for s in SIZES:
        t = d["trash"][s][l]
        row += [mib(t["A_bytes"]), mib(t.get("A_hot_bytes", 0)), mib(t.get("A_drop_bytes", 0))]
    print(f"| {l} | " + " | ".join(row) + " |")

print("\n## Short wait: peak trash held at once (GiB)\n")
print("Every replaced version waits until 5 newer copies have landed (60 to 75 minutes). Mean over 15 copy-grid offsets, min to max. Harness writes left out.\n")
print("| Chunk size | As run, all backups | As run, incrementals | Hourly, all backups | Hourly, incrementals |")
print("|---|---:|---:|---:|---:|")
for s in SIZES:
    w = d["short_wait"][s]
    cells = [f"{gib(w[k]['mean'])} ({gib(w[k]['min'])} to {gib(w[k]['max'])})" for k in ("ci_peak", "ci_peak_incrementals", "hourly_peak", "hourly_peak_incrementals")]
    print(f"| {s} | " + " | ".join(cells) + " |")

print("\n## Extrapolated to hourly backups for 7 days\n")
print(f"Per-hour trash is the mean of {', '.join(incs)}. A and B-lite hold 168 hours of it. Short wait holds at most two backups of it.\n")
print("| Chunk size | Trash per hour (MiB) | A, 7 days (GiB) | B-lite per hour (MiB) | B-lite, 7 days (GiB) | Short wait, mean 2 backups (GiB) | Short wait, worst 2 in a row (GiB) |")
print("|---|---:|---:|---:|---:|---:|---:|")
for s in SIZES:
    a = sum(d["trash"][s][l]["A_bytes"] for l in incs) / len(incs)
    b = sum(d["trash"][s][l]["B_hourly_bytes"]["mean"] for l in incs) / len(incs)
    pair = max(d["trash"][s][x]["A_bytes"] + d["trash"][s][y]["A_bytes"] for x, y in zip(incs, incs[1:]))
    print(f"| {s} | {mib(a)} | {gib(a * 168)} | {mib(b)} | {gib(b * 168)} | {gib(2 * a)} | {gib(pair)} |")

print("\n## Hot spots (8 MiB chunks, versions per backup)\n")
print("| File | Offset (MiB) | " + " | ".join(labels) + " | harness |")
print("|---|---:|" + "---:|" * (len(labels) + 1))
for h in d["hot"]["8M"][:15]:
    print(f"| `{h['file']}` | {h['offset'] // MiB} | " + " | ".join(str(h["per_backup"][l]) for l in labels) + f" | {h['per_backup']['other']} |")
for s in ("8M", "256K", "4K"):
    print(f"\n{s}: chunks with 10 or more versions per backup: " + ", ".join(f"{l} {d['hot'][s + '_hot_count'][l]} ({d['hot'][s + '_versions_in_hot'][l]} versions)" for l in labels))

print("\n## Rows at the end\n")
print("| Chunk size | Live chunk rows | Live bytes (MiB) | Files |")
print("|---|---:|---:|---:|")
for s in SIZES + ["4K"]:
    r = d["rows"][s]
    print(f"| {s} | {r['live_rows']:,} | {mib(r['live_bytes'])} | {r['files']} |")
