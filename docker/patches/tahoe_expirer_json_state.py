#!/usr/bin/env python3
"""Patch Tahoe-LAFS 1.20's lease crawler so it survives a restart mid-cycle.

The crawler saves its state as JSON after every slice. The cycle-to-date
lease-age histogram is a dict keyed by (minage, maxage) tuples, which JSON
cannot hold, so ``get_state`` writes it as a list of [minage, maxage, count]
entries. Nothing turns that list back into the dict when the state is loaded,
so the first share with a lease after a restart raises ``TypeError: list
indices must be integers or slices, not tuple`` inside the crawler's timer,
and the crawler never runs again until its state file is deleted. Upstream
master (October 2026) still has the bug; this adds the conversion to
``add_initial_state``, the hook that runs right after the state is loaded.

Applied to allmydata/storage/expirer.py while the tahoe-base image is built
(docker/Dockerfile.tahoe-base). Idempotent; fails the build loudly if the
anchor it patches is not found, so a Tahoe upgrade cannot silently drop it.
"""

from __future__ import annotations

import sys
from pathlib import Path

MARKER = "RedundaNet: a cycle saved as JSON"
ANCHOR = """        for k in so_far:
            self.state["cycle-to-date"].setdefault(k, so_far[k])
"""
ADDITION = (
    ANCHOR
    + """        # RedundaNet: a cycle saved as JSON turns the (minage, maxage)-keyed
        # histogram into a list of [minage, maxage, count]; turn it back, or
        # the first share with a lease crashes the crawler for good.
        histogram = self.state["cycle-to-date"].get("lease-age-histogram")
        if isinstance(histogram, list):
            self.state["cycle-to-date"]["lease-age-histogram"] = {
                (int(start), int(end)): int(count) for start, end, count in histogram
            }
"""
)


def patch(source: str) -> str:
    """The patched module text; unchanged when already patched."""
    if MARKER in source:
        return source
    if source.count(ANCHOR) != 1:
        raise ValueError("add_initial_state anchor not found exactly once; Tahoe changed")
    return source.replace(ANCHOR, ADDITION)


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {argv[0]} <path to allmydata/storage/expirer.py>", file=sys.stderr)
        return 2
    path = Path(argv[1])
    source = path.read_text()
    patched = patch(source)
    if patched == source:
        print(f"{path}: already patched")
        return 0
    path.write_text(patched)
    print(f"{path}: patched (lease-age histogram restored on load)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
