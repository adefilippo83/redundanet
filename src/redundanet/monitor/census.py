"""Share census: which storage objects a node holds.

Storage servers cannot read file contents, but each knows the opaque storage
indexes of the shares it stores (the directory names under shares/). Reporting
those to the hub, over the VPN only, lets the network compute per-object
replication (how many distinct servers hold each object) without anyone
revealing filenames, owners, or contents.

Tahoe share layout:  <shares_dir>/<2-char prefix>/<storage_index>/<sharenum>

The inventory is kept incrementally. Shares are immutable, so the tree only
changes when a storage index directory appears (an upload) or disappears
(garbage collection), and either changes the mtime of its prefix directory.
A refresh stats the prefix directories (at most 1,024 of them) and re-lists
only those whose mtime moved since they were last listed; a full walk of the
tree happens once, the first time, and then on a long schedule as a safety
net (a share added to an existing index directory changes only that
directory's size, which the full walk reconciles). Every change bumps a
generation, and the last changes are remembered so a reader that holds an
older generation can ask for just the difference instead of the whole list.
"""

from __future__ import annotations

import os
import shutil
import time
from collections import deque
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

CENSUS_PORT = 3459  # served on the node's VPN IP only
INVENTORY_VERSION = 1
# A prefix directory modified this close to its own listing is listed again
# next time: the listing may have raced the change.
MODIFIED_MARGIN_NS = 2 * 10**9
# Generations a node remembers for delta requests.
HISTORY_LIMIT = 1000
# A change bigger than this (the first walk, a disk swap) is not kept as a
# delta: a reader behind it fetches the full inventory instead.
HISTORY_MAX_CHANGE = 50_000


def scan_prefix(prefix_dir: Path) -> dict[str, int]:
    """The storage indexes under one prefix directory that have at least one
    share file on disk, each with the bytes of its share files."""
    found: dict[str, int] = {}
    for si_dir in _subdirs(prefix_dir):
        total = 0
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
            found[si_dir.name] = total
    return found


def scan_shares(shares_dir: Path) -> tuple[list[str], int]:
    """One full pass over the shares tree: (storage indexes with at least one
    share on disk, bytes of every file under the tree).

    ``incoming/`` (Tahoe's staging area for uploads in flight) counts toward
    the bytes but never toward the indexes.
    """
    indexes: list[str] = []
    total = 0
    if not shares_dir.is_dir():
        return indexes, total
    for prefix in sorted(_subdirs(shares_dir), key=lambda e: e.name):
        if prefix.name == "incoming":
            total += _tree_bytes(Path(prefix.path))
            continue
        found = scan_prefix(Path(prefix.path))
        indexes.extend(sorted(found))
        total += sum(found.values())
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


# --- the incremental inventory -----------------------------------------------


@dataclass
class Prefix:
    """One prefix directory as last listed."""

    mtime_ns: int  # the directory's mtime at that listing
    listed_at_ns: int  # the clock at that listing
    indexes: dict[str, int] = field(default_factory=dict)  # storage index -> share bytes


@dataclass(frozen=True)
class Change:
    generation: int
    added: frozenset[str]
    removed: frozenset[str]


@dataclass(frozen=True)
class Walk:
    """What one refresh did."""

    mode: str  # "full" or "incremental"
    seconds: float
    prefixes: int  # prefix directories present
    listed: int  # of which re-listed this time
    added: int
    removed: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "seconds": round(self.seconds, 2),
            "prefixes": self.prefixes,
            "listed": self.listed,
            "added": self.added,
            "removed": self.removed,
        }


class Inventory:
    """The storage indexes a node holds, refreshed incrementally."""

    def __init__(self) -> None:
        self.prefixes: dict[str, Prefix] = {}
        self.incoming_bytes = 0
        self.generation = 0
        self.history: deque[Change] = deque(maxlen=HISTORY_LIMIT)
        self.full_walk_at_ns: int | None = None

    @property
    def object_count(self) -> int:
        return sum(len(p.indexes) for p in self.prefixes.values())

    @property
    def disk_used_bytes(self) -> int:
        return self.incoming_bytes + sum(sum(p.indexes.values()) for p in self.prefixes.values())

    def storage_indexes(self) -> list[str]:
        """Sorted, the order ``scan_shares`` gives."""
        indexes: list[str] = []
        for name in sorted(self.prefixes):
            indexes.extend(sorted(self.prefixes[name].indexes))
        return indexes

    def refresh(self, shares_dir: Path, now_ns: int | None = None, full: bool = False) -> Walk:
        """Bring the inventory up to date with the tree.

        ``full`` re-lists every prefix. Otherwise a prefix is re-listed when
        it is new, when its mtime differs from the last listing, or when it
        was modified within MODIFIED_MARGIN_NS of that listing.
        """
        now_ns = time.time_ns() if now_ns is None else now_ns
        started = time.monotonic()
        mode = "full" if full or not self.prefixes else "incremental"
        added: set[str] = set()
        removed: set[str] = set()
        seen: set[str] = set()
        listed = 0
        incoming = 0
        for entry in _subdirs(shares_dir) if shares_dir.is_dir() else []:
            if entry.name == "incoming":
                incoming = _tree_bytes(Path(entry.path))
                continue
            seen.add(entry.name)
            try:
                mtime_ns = entry.stat(follow_symlinks=False).st_mtime_ns
            except OSError:
                continue
            known = self.prefixes.get(entry.name)
            if not (
                mode == "full"
                or known is None
                or mtime_ns != known.mtime_ns
                or known.mtime_ns >= known.listed_at_ns - MODIFIED_MARGIN_NS
            ):
                continue
            indexes = scan_prefix(Path(entry.path))
            listed += 1
            before = known.indexes.keys() if known else set()
            added.update(indexes.keys() - before)
            removed.update(before - indexes.keys())
            self.prefixes[entry.name] = Prefix(mtime_ns, now_ns, indexes)
        for name in list(self.prefixes.keys() - seen):
            removed.update(self.prefixes.pop(name).indexes)
        self.incoming_bytes = incoming
        if added or removed:
            self.generation += 1
            if len(added) + len(removed) > HISTORY_MAX_CHANGE:
                self.history.clear()
            else:
                self.history.append(Change(self.generation, frozenset(added), frozenset(removed)))
        if mode == "full":
            self.full_walk_at_ns = now_ns
        return Walk(
            mode=mode,
            seconds=time.monotonic() - started,
            prefixes=len(seen),
            listed=listed,
            added=len(added),
            removed=len(removed),
        )

    def changes_since(self, since: int) -> tuple[set[str], set[str]] | None:
        """(added, removed) between generation ``since`` and now, net of
        changes that undid each other; None when ``since`` is not a
        generation this inventory can still account for."""
        if since == self.generation:
            return set(), set()
        if since > self.generation or not self.history or self.history[0].generation > since + 1:
            return None
        added: set[str] = set()
        removed: set[str] = set()
        for change in self.history:
            if change.generation <= since:
                continue
            for si in change.added:
                if si in removed:
                    removed.discard(si)
                else:
                    added.add(si)
            for si in change.removed:
                if si in added:
                    added.discard(si)
                else:
                    removed.add(si)
        return added, removed

    def to_json(self) -> dict[str, Any]:
        return {
            "version": INVENTORY_VERSION,
            "generation": self.generation,
            "full_walk_at_ns": self.full_walk_at_ns,
            "incoming_bytes": self.incoming_bytes,
            "prefixes": {
                name: {"mtime_ns": p.mtime_ns, "listed_at_ns": p.listed_at_ns, "indexes": p.indexes}
                for name, p in self.prefixes.items()
            },
            "history": [
                {"generation": c.generation, "added": sorted(c.added), "removed": sorted(c.removed)}
                for c in self.history
            ],
        }

    @classmethod
    def from_json(cls, data: Any) -> Inventory:
        """Rebuild a persisted inventory; ValueError when it is not one."""
        if not isinstance(data, dict) or data.get("version") != INVENTORY_VERSION:
            raise ValueError("not a census inventory of this version")
        try:
            inventory = cls()
            inventory.generation = int(data["generation"])
            full = data.get("full_walk_at_ns")
            inventory.full_walk_at_ns = None if full is None else int(full)
            inventory.incoming_bytes = int(data.get("incoming_bytes", 0))
            for name, raw in data["prefixes"].items():
                inventory.prefixes[str(name)] = Prefix(
                    mtime_ns=int(raw["mtime_ns"]),
                    listed_at_ns=int(raw["listed_at_ns"]),
                    indexes={str(si): int(size) for si, size in raw["indexes"].items()},
                )
            for raw in data.get("history", []):
                inventory.history.append(
                    Change(
                        int(raw["generation"]),
                        frozenset(str(si) for si in raw["added"]),
                        frozenset(str(si) for si in raw["removed"]),
                    )
                )
        except (KeyError, TypeError, ValueError, AttributeError) as e:
            raise ValueError(f"malformed census inventory: {e}") from e
        return inventory


# --- payloads ----------------------------------------------------------------


def _meta(
    node_name: str, inventory: Inventory, shares_dir: Path, walk: Walk | None, now: datetime | None
) -> dict[str, Any]:
    total, free = disk_capacity(shares_dir)
    meta: dict[str, Any] = {
        "node": node_name,
        "generation": inventory.generation,
        "object_count": inventory.object_count,
        "disk_used_bytes": inventory.disk_used_bytes,
        "disk_total_bytes": total,
        "disk_free_bytes": free,
        "computed_at": (now or datetime.now(UTC)).isoformat(timespec="seconds"),
    }
    if walk is not None:
        meta["walk"] = walk.to_dict()
    return meta


def inventory_payload(
    node_name: str,
    inventory: Inventory,
    shares_dir: Path,
    walk: Walk | None = None,
    now: datetime | None = None,
) -> dict[str, Any]:
    """The full JSON body served at /census: every storage index."""
    payload = _meta(node_name, inventory, shares_dir, walk, now)
    payload["storage_indexes"] = inventory.storage_indexes()
    return payload


def delta_payload(
    node_name: str,
    inventory: Inventory,
    since: int,
    shares_dir: Path,
    walk: Walk | None = None,
    now: datetime | None = None,
) -> dict[str, Any] | None:
    """The body served at /census?since=<generation>: only what changed
    since that generation. None when the node cannot account for it, so
    the caller serves the full inventory instead."""
    changes = inventory.changes_since(since)
    if changes is None:
        return None
    added, removed = changes
    payload = _meta(node_name, inventory, shares_dir, walk, now)
    payload["base"] = since
    payload["added"] = sorted(added)
    payload["removed"] = sorted(removed)
    return payload


def census_payload(node_name: str, shares_dir: Path, now: datetime | None = None) -> dict[str, Any]:
    """The full census from one walk of the tree (no inventory kept)."""
    inventory = Inventory()
    walk = inventory.refresh(shares_dir)
    return inventory_payload(node_name, inventory, shares_dir, walk, now)
