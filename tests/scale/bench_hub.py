#!/usr/bin/env python3
"""Scale benchmark: the hub's status collection at N objects per storage node.

Generates the censuses that ``nodes`` storage nodes would serve when every
object is stored on ``copies`` of them (so the grid holds N * nodes / copies
distinct objects), writes them to disk as the JSON bodies the nodes serve,
and then, in a fresh interpreter, runs the hub's ``collect_status`` the way
``status_server.py`` does: a census fetcher that parses each body once and
keeps the parsed payload (the ``If-None-Match`` path), wrapped in the
``ThrottledFetcher``; the census cache directory, so ``save_census`` costs
are included; ``render_html`` and the JSON snapshot afterwards. Reports per
tick wall time and the process RSS, which is what the 512 MB fly machine
runs out of.

    poetry run python tests/scale/bench_hub.py --objects-per-node 250000
    poetry run python tests/scale/bench_hub.py --objects-per-node 1000000 --nodes 8 --copies 4

Not a pytest test. ``scripts/scale-test.sh`` drives it.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import subprocess
import sys
import tempfile
import time
from datetime import UTC, datetime
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "src"))


def rss_mb() -> float:
    try:
        with open("/proc/self/statm") as f:
            return int(f.read().split()[1]) * os.sysconf("SC_PAGE_SIZE") / 1e6
    except (OSError, ValueError):
        out = subprocess.run(
            ["ps", "-o", "rss=", "-p", str(os.getpid())], capture_output=True, text=True
        )
        return int(out.stdout.strip() or 0) * 1024 / 1e6


def node_name(i: int) -> str:
    return f"node-{i:08x}"


def node_ip(i: int) -> str:
    return f"10.200.0.{10 + i}"


def generate(out: Path, per_node: int, nodes: int, copies: int) -> dict[str, int]:
    """Write ``nodes`` census bodies. Object i lives on nodes i, i+1, ...,
    i+copies-1 (mod nodes), so every node holds exactly ``per_node``."""
    total = per_node * nodes // copies
    started = time.monotonic()
    holders: list[list[str]] = [[] for _ in range(nodes)]
    for i in range(total):
        si = base64.b32encode(os.urandom(16)).decode().lower()[:26]
        for j in range(copies):
            holders[(i + j) % nodes].append(si)
        if i and i % 500000 == 0:
            print(
                f"  generated {i}/{total} objects ({time.monotonic() - started:.0f}s)", flush=True
            )
    out.mkdir(parents=True, exist_ok=True)
    for i, indexes in enumerate(holders):
        payload = {
            "node": node_name(i),
            "object_count": len(indexes),
            "storage_indexes": indexes,
            "disk_used_bytes": len(indexes) * 300000,
            "disk_total_bytes": 10**12,
            "disk_free_bytes": 10**12 - len(indexes) * 300000,
            "computed_at": datetime.now(UTC).isoformat(timespec="seconds"),
        }
        (out / f"{node_name(i)}.json").write_bytes(json.dumps(payload).encode())
        holders[i] = []
    return {"objects_total": total, "generate_s": round(time.monotonic() - started, 1)}


def measure(bodies: Path, nodes: int, copies: int, ticks: int, churn: bool, cache: bool) -> None:
    """Runs in a fresh interpreter: only the hub's own memory is measured."""
    from redundanet.monitor.render import render_html
    from redundanet.monitor.status import ThrottledFetcher, collect_status

    manifest = {
        "network": {"tahoe": {"shares_needed": 2, "shares_happy": copies, "shares_total": copies}},
        "nodes": [
            {
                "name": node_name(i),
                "vpn_ip": node_ip(i),
                "roles": ["tahoe_storage"],
                "status": "active",
            }
            for i in range(nodes)
        ],
    }
    ip_to_body = {node_ip(i): bodies / f"{node_name(i)}.json" for i in range(nodes)}

    class Fetcher:
        """status_server.ConditionalCensusFetcher without the socket: the
        first call parses the body; later calls return the parsed payload
        (the node answered 304), unless ``churn`` re-parses every time (the
        node's inventory changed)."""

        def __init__(self) -> None:
            self.known: dict[str, dict] = {}
            self.fetches = 0
            self.fetch_s = 0.0

        def __call__(self, vpn_ip: str) -> dict | None:
            if not churn and vpn_ip in self.known:
                return self.known[vpn_ip]
            started = time.monotonic()
            payload = json.loads(ip_to_body[vpn_ip].read_bytes())
            self.fetch_s += time.monotonic() - started
            self.fetches += 1
            self.known[vpn_ip] = payload
            return payload

    fetcher = Fetcher()
    throttled = ThrottledFetcher(fetcher, ttl=0 if churn else 300)
    cache_dir = Path(tempfile.mkdtemp(prefix="hub-census-cache-")) if cache else None
    base_rss = rss_mb()
    print(
        f"hub baseline rss {base_rss:.0f} MB; cache_dir={'on' if cache else 'off'}; churn={churn}"
    )
    for tick in range(1, ticks + 1):
        fetcher.fetches, fetcher.fetch_s = 0, 0.0
        started = time.monotonic()
        status = collect_status(
            manifest=manifest,
            self_name="hub",
            ping=lambda _ip: 1.0,
            storage_connected=nodes,
            furl_present=True,
            manifest_synced_at=datetime.now(UTC),
            fetch_census=throttled,
            census_cache_dir=cache_dir,
        )
        collect_s = time.monotonic() - started
        started = time.monotonic()
        html = render_html(status)
        body = json.dumps(status.to_dict(), indent=1).encode()
        render_s = time.monotonic() - started
        rep = status.replication
        print(
            json.dumps(
                {
                    "tick": tick,
                    "collect_s": round(collect_s, 2),
                    "of_which_parse_s": round(fetcher.fetch_s, 2),
                    "bodies_parsed": fetcher.fetches,
                    "render_s": round(render_s, 2),
                    "rss_mb": round(rss_mb(), 0),
                    "objects_total": rep.objects_total if rep else None,
                    "fully_replicated": rep.fully_replicated if rep else None,
                    "html_kb": len(html) // 1000,
                    "json_kb": len(body) // 1000,
                }
            ),
            flush=True,
        )
    if cache_dir is not None:
        size = sum(p.stat().st_size for p in cache_dir.iterdir())
        print(f"census cache dir: {size / 1e6:.1f} MB written per live tick")
        subprocess.run(["rm", "-rf", str(cache_dir)], check=False)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--objects-per-node", type=int, required=True)
    parser.add_argument("--nodes", type=int, default=8)
    parser.add_argument("--copies", type=int, default=4)
    parser.add_argument("--ticks", type=int, default=3)
    parser.add_argument("--churn", action="store_true", help="every tick re-parses every census")
    parser.add_argument("--no-cache", action="store_true", help="skip the census cache dir writes")
    parser.add_argument("--root", type=Path, default=Path(os.environ.get("SCALE_ROOT", "/tmp")))
    parser.add_argument("--_measure", type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args()

    if args._measure:
        measure(args._measure, args.nodes, args.copies, args.ticks, args.churn, not args.no_cache)
        return 0

    bodies = args.root / f"hub-{args.objects_per_node}x{args.nodes}c{args.copies}"
    if not (bodies / f"{node_name(args.nodes - 1)}.json").is_file():
        print(f"generating {args.nodes} censuses of {args.objects_per_node} objects", flush=True)
        print(json.dumps(generate(bodies, args.objects_per_node, args.nodes, args.copies)))
    total_bytes = sum(p.stat().st_size for p in bodies.iterdir())
    print(f"census bodies on disk: {total_bytes / 1e6:.1f} MB for {args.nodes} nodes")
    cmd = [
        sys.executable,
        __file__,
        "--objects-per-node",
        str(args.objects_per_node),
        "--nodes",
        str(args.nodes),
        "--copies",
        str(args.copies),
        "--ticks",
        str(args.ticks),
        "--_measure",
        str(bodies),
    ]
    if args.churn:
        cmd.append("--churn")
    if args.no_cache:
        cmd.append("--no-cache")
    return subprocess.run(cmd, check=False).returncode


if __name__ == "__main__":
    sys.exit(main())
