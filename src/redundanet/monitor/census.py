"""Share census: which storage objects a node holds.

Storage servers cannot read file contents, but each knows the opaque storage
indexes of the shares it stores (the directory names under shares/). Reporting
those to the hub — over the VPN only — lets the network compute per-object
replication (how many distinct servers hold each object) without anyone
revealing filenames, owners, or contents.

Tahoe share layout:  <shares_dir>/<2-char prefix>/<storage_index>/<sharenum>
"""

from __future__ import annotations

import os
import shutil
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

CENSUS_PORT = 3459  # served on the node's VPN IP only


def scan_shares(shares_dir: Path) -> tuple[list[str], int]:
    """One pass over the shares tree: (storage indexes with at least one
    share on disk, bytes of every file under the tree).

    A single ``scandir`` walk, because on a node with a hundred thousand
    objects the tree is the cost: listing the indexes and summing the sizes
    in separate walks doubled it, and a Pi's USB disk needs seconds per
    walk. ``incoming/`` (Tahoe's staging area for uploads in flight) counts
    toward the bytes but never toward the indexes.
    """
    indexes: list[str] = []
    total = 0
    if not shares_dir.is_dir():
        return indexes, total
    for prefix in sorted(_subdirs(shares_dir), key=lambda e: e.name):
        if prefix.name == "incoming":
            total += _tree_bytes(Path(prefix.path))
            continue
        for si_dir in sorted(_subdirs(Path(prefix.path)), key=lambda e: e.name):
            has_share = False
            try:
                with os.scandir(si_dir.path) as entries:
                    for entry in entries:
                        if entry.is_file(follow_symlinks=False):
                            has_share = True
                            total += entry.stat(follow_symlinks=False).st_size
            except OSError:
                continue
            if has_share:
                indexes.append(si_dir.name)
    return indexes, total


def _subdirs(path: Path) -> list[os.DirEntry[str]]:
    try:
        with os.scandir(path) as entries:
            return [e for e in entries if e.is_dir(follow_symlinks=False)]
    except OSError:
        return []


def _tree_bytes(path: Path) -> int:
    total = 0
    try:
        with os.scandir(path) as entries:
            for entry in entries:
                if entry.is_file(follow_symlinks=False):
                    total += entry.stat(follow_symlinks=False).st_size
                elif entry.is_dir(follow_symlinks=False):
                    total += _tree_bytes(Path(entry.path))
    except OSError:
        pass
    return total


def list_storage_indexes(shares_dir: Path) -> list[str]:
    """All storage indexes with at least one share on disk."""
    return scan_shares(shares_dir)[0]


def disk_used_bytes(shares_dir: Path) -> int:
    """Total bytes of stored shares."""
    return scan_shares(shares_dir)[1]


def disk_capacity(shares_dir: Path) -> tuple[int | None, int | None]:
    """(total, free) bytes of the filesystem holding the shares, or Nones.

    Lets the hub verify a node's claimed ``storage_contribution`` against the
    disk it actually has.
    """
    try:
        usage = shutil.disk_usage(shares_dir if shares_dir.exists() else shares_dir.parent)
    except OSError:
        return None, None
    return usage.total, usage.free


def census_payload(node_name: str, shares_dir: Path, now: datetime | None = None) -> dict[str, Any]:
    """The JSON body served at /census. ``computed_at`` tells the reader how
    old the inventory is: the node computes it on a schedule, not per request."""
    indexes, used = scan_shares(shares_dir)
    total, free = disk_capacity(shares_dir)
    return {
        "node": node_name,
        "object_count": len(indexes),
        "storage_indexes": indexes,
        "disk_used_bytes": used,
        "disk_total_bytes": total,
        "disk_free_bytes": free,
        "computed_at": (now or datetime.now(UTC)).isoformat(timespec="seconds"),
    }
