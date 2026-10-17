#!/usr/bin/env python3
"""Scale benchmark: the storage node's share census at N objects.

Builds a synthetic Tahoe shares tree (``<prefix>/<storage index>/0``, one
share per object) and times exactly what ``share_census.refresh`` does on a
node: the walk (``scan_shares``), the JSON body, the ETag digest. Then the
hub's side of the same bytes: ``json.loads`` and the memory the parsed list
occupies, which is what the hub keeps per node between refreshes.

    poetry run python tests/scale/bench_census.py --objects 250000
    poetry run python tests/scale/bench_census.py --objects 1000000 --root /mnt/disk

The tree is reused when it already exists (a marker records its size), so
the generation cost is paid once. ``--drop-caches`` empties the page cache
before the first walk (Linux, root only) to measure a cold walk: on a node
that just booted, or whose cache the shares no longer fit in, that is the
walk the hub waits for.

Not a pytest test: it takes minutes and needs disk. ``scripts/scale-test.sh``
drives it.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import resource
import subprocess
import sys
import time
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))
sys.path.insert(0, str(REPO_ROOT / "src"))

import share_census  # noqa: E402

from redundanet.monitor.census import census_payload, scan_shares  # noqa: E402

MARKER = "SCALE-TREE-COMPLETE"


def rss_mb() -> float:
    """Resident set size of this process, in MB."""
    try:
        with open("/proc/self/statm") as f:
            return int(f.read().split()[1]) * os.sysconf("SC_PAGE_SIZE") / 1e6
    except (OSError, ValueError):
        out = subprocess.run(
            ["ps", "-o", "rss=", "-p", str(os.getpid())], capture_output=True, text=True
        )
        return int(out.stdout.strip() or 0) * 1024 / 1e6


def peak_rss_mb() -> float:
    peak = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    # Linux reports KB, macOS bytes.
    return peak / 1e3 if sys.platform.startswith("linux") else peak / 1e6


def storage_index() -> str:
    return base64.b32encode(os.urandom(16)).decode().lower()[:26]


def build_tree(shares: Path, objects: int, share_bytes: int) -> float:
    """Create the tree unless a marker says it is already complete.
    Returns the seconds spent (0 when reused)."""
    marker = shares.parent / MARKER
    if marker.is_file() and marker.read_text().strip() == str(objects):
        return 0.0
    if shares.exists():
        subprocess.run(["rm", "-rf", str(shares)], check=True)
    started = time.monotonic()
    alphabet = "abcdefghijklmnopqrstuvwxyz234567"
    for a in alphabet:
        for b in alphabet:
            (shares / f"{a}{b}").mkdir(parents=True, exist_ok=True)
    payload = b"\0" * share_bytes
    made = 0
    while made < objects:
        si = storage_index()
        si_dir = shares / si[:2] / si
        try:
            si_dir.mkdir()
        except FileExistsError:
            continue
        with open(si_dir / "0", "wb") as f:
            f.write(payload)
        made += 1
        if made % 100000 == 0:
            print(f"  generated {made}/{objects} ({time.monotonic() - started:.0f}s)", flush=True)
    marker.write_text(f"{objects}\n")
    return time.monotonic() - started


def drop_caches() -> bool:
    try:
        subprocess.run(["sync"], check=True)
        with open("/proc/sys/vm/drop_caches", "w") as f:
            f.write("3\n")
        return True
    except (OSError, subprocess.CalledProcessError):
        return False


def timed(fn, *args):
    started = time.monotonic()
    result = fn(*args)
    return result, time.monotonic() - started


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--objects", type=int, required=True)
    parser.add_argument("--root", type=Path, default=Path(os.environ.get("SCALE_ROOT", "/tmp")))
    parser.add_argument("--share-bytes", type=int, default=64)
    parser.add_argument("--drop-caches", action="store_true")
    parser.add_argument("--json", action="store_true", help="print one JSON line only")
    args = parser.parse_args()

    tree = args.root / f"census-{args.objects}"
    shares = tree / "shares"
    gen_s = build_tree(shares, args.objects, args.share_bytes)
    result: dict[str, object] = {"objects": args.objects, "generate_s": round(gen_s, 1)}

    result["cold"] = args.drop_caches and drop_caches()
    (indexes, _used), walk1 = timed(scan_shares, shares)
    assert len(indexes) == args.objects, (len(indexes), args.objects)
    _, walk2 = timed(scan_shares, shares)
    result["walk_first_s"] = round(walk1, 2)
    result["walk_warm_s"] = round(walk2, 2)

    payload, payload_s = timed(census_payload, "bench", shares)
    body, dumps_s = timed(lambda p: json.dumps(p).encode(), payload)
    tagged = {k: v for k, v in payload.items() if k not in share_census.VOLATILE_FIELDS}
    _, etag_s = timed(
        lambda t: hashlib.sha256(json.dumps(t, sort_keys=True).encode()).hexdigest(), tagged
    )
    share_census.Handler.latest = b""
    _, refresh_s = timed(share_census.refresh, "bench", shares)
    result.update(
        payload_s=round(payload_s, 2),
        dumps_s=round(dumps_s, 2),
        etag_s=round(etag_s, 2),
        refresh_total_s=round(refresh_s, 2),
        json_bytes=len(body),
        bytes_per_object=round(len(body) / args.objects, 1),
        node_rss_mb=round(rss_mb(), 1),
    )
    del indexes, payload, tagged
    share_census.Handler.latest = b""

    # The hub's side: parse the body and keep the list, as the fetcher does.
    before = rss_mb()
    parsed, parse_s = timed(json.loads, body)
    held = sum(sys.getsizeof(s) for s in parsed["storage_indexes"]) + sys.getsizeof(
        parsed["storage_indexes"]
    )
    result.update(
        hub_parse_s=round(parse_s, 2),
        hub_parsed_mb=round(held / 1e6, 1),
        hub_rss_delta_mb=round(rss_mb() - before, 1),
        peak_rss_mb=round(peak_rss_mb(), 1),
    )
    del parsed

    if args.json:
        print(json.dumps(result))
    else:
        width = max(len(k) for k in result)
        for key, value in result.items():
            print(f"{key:<{width}}  {value}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
