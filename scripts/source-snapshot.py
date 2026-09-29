#!/usr/bin/env python3
"""Verify/reconstruct pinned source selection without overwriting local changes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]


def digest(data):
    return hashlib.sha256(data).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, help="reconstruct baseline in a new empty directory")
    parser.add_argument("--cache", type=Path, help="read an existing Go module cache instead of downloading")
    args = parser.parse_args()
    manifest = json.loads((ROOT / "docs/source-manifest.json").read_text())
    if args.output:
        args.output = args.output.resolve()
        if args.output.exists() and any(args.output.iterdir()):
            parser.error("--output must be empty; never reconstruct over application patches")
        args.output.mkdir(parents=True, exist_ok=True)
    sources = {}
    env = dict(os.environ, GOWORK="off", GOMAXPROCS="2")
    for name, spec in manifest["snapshots"].items():
        version = spec["source"] + "@" + spec["version"]
        if args.cache:
            sources[name] = args.cache / version
        else:
            result = subprocess.run(["go", "mod", "download", "-json", version],
                                    cwd=ROOT, env=env, check=True, capture_output=True, text=True)
            meta = json.loads(result.stdout)
            for field, key in [("Sum", "sum"), ("GoModSum", "go_mod_sum")]:
                if key in spec and meta.get(field) != spec[key]:
                    raise ValueError(f"{version}: {field} mismatch")
            if spec.get("commit") and meta.get("Origin", {}).get("Hash", spec["commit"]) != spec["commit"]:
                raise ValueError(f"{version}: source commit mismatch")
            sources[name] = Path(meta["Dir"])
    changed = []
    for item in manifest["files"]:
        source = sources[item["module"]] / item["source"]
        data = source.read_bytes()
        if digest(data) != item["sha256"]:
            raise ValueError(f"{source}: original source hash mismatch")
        if source.suffix == ".go":
            for old, new in manifest["imports"].items():
                data = data.replace(old.encode(), new.encode())
        if digest(data) != item["relocated_sha256"]:
            raise ValueError(f"{source}: import relocation mismatch")
        local = ROOT / item["destination"]
        if not local.is_file():
            raise ValueError(f"missing selected source: {local}")
        if local.read_bytes() != data:
            changed.append(item["destination"])
        if args.output:
            dest = args.output / item["destination"]
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(data)
    print(f"Verified {len(manifest['files'])} selected files from {len(sources)} pinned snapshots.")
    print("Application-modified upstream files (review against docs/packaging.md):")
    for name in changed:
        print(name)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        print(exc, file=sys.stderr)
        sys.exit(1)
