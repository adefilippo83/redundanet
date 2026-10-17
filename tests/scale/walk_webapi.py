#!/usr/bin/env python3
"""Walk an alias through the node's web API instead of one ``tahoe ls``
process per directory: the candidate replacement for the usage meter's and
the rebalancer's directory walks, measured against them.

Runs inside a tahoe-client container (stdlib only):

    python3 walk_webapi.py backups:

Same traversal as ``redundanet.storage.inventory.walk_files``: directories
are visited once by capability, files are collected, nothing is downloaded.
"""

from __future__ import annotations

import json
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

NODE_DIR = Path("/var/lib/tahoe-client")


def alias_cap(alias: str) -> str:
    for line in (NODE_DIR / "private" / "aliases").read_text().splitlines():
        name, _, cap = line.partition(":")
        if name.strip() == alias.rstrip(":"):
            return cap.strip()
    raise SystemExit(f"no alias {alias!r}")


def main() -> int:
    alias = sys.argv[1] if len(sys.argv) > 1 else "backups:"
    node_url = (NODE_DIR / "node.url").read_text().strip()
    root = alias_cap(alias)
    started = time.monotonic()
    seen_dirs = {root}
    pending = [root]
    files = 0
    requests = 0
    while pending:
        cap = pending.pop()
        url = f"{node_url}uri/{urllib.parse.quote(cap, safe='')}?t=json"
        with urllib.request.urlopen(url, timeout=900) as response:  # noqa: S310 - node.url
            node_type, payload = json.load(response)
        requests += 1
        if node_type != "dirnode":
            continue
        for _name, (child_type, child) in (payload.get("children") or {}).items():
            child_cap = child.get("rw_uri") or child.get("ro_uri") or ""
            if child_type == "dirnode":
                if child_cap and child_cap not in seen_dirs:
                    seen_dirs.add(child_cap)
                    pending.append(child_cap)
            elif child_type == "filenode":
                files += 1
    elapsed = time.monotonic() - started
    print(
        json.dumps(
            {
                "alias": alias,
                "directories": len(seen_dirs),
                "files": files,
                "requests": requests,
                "seconds": round(elapsed, 1),
                "ms_per_request": round(1000 * elapsed / max(requests, 1), 1),
            }
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
