"""Share census: which storage objects a node holds.

Storage servers cannot read file contents, but each knows the opaque storage
indexes of the shares it stores (the directory names under shares/). Reporting
those to the hub — over the VPN only — lets the network compute per-object
replication (how many distinct servers hold each object) without anyone
revealing filenames, owners, or contents.

Tahoe share layout:  <shares_dir>/<2-char prefix>/<storage_index>/<sharenum>
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

CENSUS_PORT = 3459  # served on the node's VPN IP only


def list_storage_indexes(shares_dir: Path) -> list[str]:
    """All storage indexes with at least one share on disk."""
    indexes: list[str] = []
    if not shares_dir.is_dir():
        return indexes
    for prefix in sorted(shares_dir.iterdir()):
        # Skip Tahoe's staging area and stray files.
        if not prefix.is_dir() or prefix.name == "incoming":
            continue
        for si_dir in sorted(prefix.iterdir()):
            if si_dir.is_dir() and any(p.is_file() for p in si_dir.iterdir()):
                indexes.append(si_dir.name)
    return indexes


def disk_used_bytes(shares_dir: Path) -> int:
    """Total bytes of stored shares."""
    total = 0
    if not shares_dir.is_dir():
        return 0
    for path in shares_dir.rglob("*"):
        if path.is_file():
            try:
                total += path.stat().st_size
            except OSError:
                continue
    return total


def census_payload(node_name: str, shares_dir: Path) -> dict[str, Any]:
    """The JSON body served at /census."""
    indexes = list_storage_indexes(shares_dir)
    return {
        "node": node_name,
        "object_count": len(indexes),
        "storage_indexes": indexes,
        "disk_used_bytes": disk_used_bytes(shares_dir),
    }
