"""The hub's copy of every storage node's share census, kept in SQLite.

The status page needs, for every object on the grid, how many storage nodes
hold it. Holding each node's list of storage indexes as Python objects costs
over a hundred bytes per entry and grows with the grid until the hub's small
machine runs out of memory; a table of (storage index, node) rows costs a
bounded cache and one GROUP BY query, and it survives a hub restart on the
volume, so an unreachable node keeps its last known inventory without any
separate cache file.

Nodes that serve generations (``/census?since=``) send only what changed and
the delta is applied as inserts and deletes; a node that still serves the
full list is replaced row for row when its payload actually changed. The
replication summary is recomputed only after something changed.

Pure with respect to time: every method that records a time takes it as a
parameter.
"""

from __future__ import annotations

import json
import sqlite3
from collections.abc import Iterable
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any

SCHEMA = """
CREATE TABLE IF NOT EXISTS nodes (
    name TEXT PRIMARY KEY,
    vpn_ip TEXT,
    generation INTEGER,
    fingerprint TEXT,
    object_count INTEGER NOT NULL DEFAULT 0,
    disk_used_bytes INTEGER NOT NULL DEFAULT 0,
    disk_total_bytes INTEGER,
    disk_free_bytes INTEGER,
    computed_at TEXT,
    fetched_at REAL NOT NULL,
    live INTEGER NOT NULL DEFAULT 1,
    fetch_seconds REAL,
    walk_seconds REAL,
    walk_mode TEXT
);
CREATE TABLE IF NOT EXISTS shares (
    si TEXT NOT NULL,
    node TEXT NOT NULL,
    PRIMARY KEY (si, node)
) WITHOUT ROWID;
"""


@dataclass(frozen=True)
class NodeRecord:
    name: str
    vpn_ip: str | None
    generation: int | None
    object_count: int
    disk_used_bytes: int
    disk_total_bytes: int | None
    disk_free_bytes: int | None
    computed_at: str | None
    fetched_at: float
    live: bool
    fetch_seconds: float | None
    walk_seconds: float | None
    walk_mode: str | None


@dataclass(frozen=True)
class Summary:
    objects_total: int
    fully_replicated: int  # on at least ``target`` nodes, counting unreachable ones
    available_full: int  # on at least ``target`` nodes that answered this cycle
    unreadable_now: int  # fewer live copies than ``needed``


def unchanged_marker(_payload: dict[str, Any]) -> dict[str, Any]:
    """What a throttled fetcher remembers between refreshes instead of the
    payload: the node is still there, nothing new to apply."""
    return {"node_unchanged": True}


class CensusStore:
    def __init__(self, path: Path | str = ":memory:") -> None:
        # Opened by the process at start, used by the collector thread: one
        # thread at a time, not necessarily the one that opened it.
        self._conn = sqlite3.connect(str(path), check_same_thread=False)
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA synchronous=NORMAL")
        self._conn.execute("PRAGMA cache_size=-16000")  # 16 MB, the memory ceiling
        self._conn.executescript(SCHEMA)
        self._dirty = True
        self._summary: tuple[tuple[Any, ...], Summary] | None = None

    def close(self) -> None:
        self._conn.close()

    # --- reading ---------------------------------------------------------

    def node(self, name: str) -> NodeRecord | None:
        row = self._conn.execute(
            "SELECT name, vpn_ip, generation, object_count, disk_used_bytes, disk_total_bytes,"
            " disk_free_bytes, computed_at, fetched_at, live, fetch_seconds, walk_seconds,"
            " walk_mode FROM nodes WHERE name = ?",
            (name,),
        ).fetchone()
        return None if row is None else _record(row)

    def records(self) -> dict[str, NodeRecord]:
        rows = self._conn.execute(
            "SELECT name, vpn_ip, generation, object_count, disk_used_bytes, disk_total_bytes,"
            " disk_free_bytes, computed_at, fetched_at, live, fetch_seconds, walk_seconds,"
            " walk_mode FROM nodes"
        ).fetchall()
        return {row[0]: _record(row) for row in rows}

    def generation_for_address(self, vpn_ip: str) -> int | None:
        """The generation held for the node at this address, for a
        ``?since=`` request; None when a full inventory is needed."""
        row = self._conn.execute(
            "SELECT generation FROM nodes WHERE vpn_ip = ? AND generation IS NOT NULL", (vpn_ip,)
        ).fetchone()
        return None if row is None else int(row[0])

    def rows(self) -> int:
        return int(self._conn.execute("SELECT count(*) FROM shares").fetchone()[0])

    def summary(self, target: int, needed: int) -> Summary:
        """Per-object replication over every stored node; cached until the
        next change."""
        key = (target, needed)
        if not self._dirty and self._summary is not None and self._summary[0] == key:
            return self._summary[1]
        placed = self._conn.execute(
            "SELECT c, count(*) FROM (SELECT si, count(*) AS c FROM shares GROUP BY si) GROUP BY c"
        ).fetchall()
        live = self._conn.execute(
            "SELECT c, count(*) FROM (SELECT si, count(*) AS c FROM shares"
            " WHERE node IN (SELECT name FROM nodes WHERE live = 1) GROUP BY si) GROUP BY c"
        ).fetchall()
        objects_total = sum(n for _c, n in placed)
        summary = Summary(
            objects_total=objects_total,
            fully_replicated=sum(n for c, n in placed if c >= target),
            available_full=sum(n for c, n in live if c >= target),
            unreadable_now=objects_total - sum(n for c, n in live if c >= needed),
        )
        self._summary = (key, summary)
        self._dirty = False
        return summary

    # --- writing ---------------------------------------------------------

    def ingest(self, name: str, vpn_ip: str, payload: dict[str, Any], now: float) -> str:
        """Apply what a node answered. Returns what happened: ``full`` (the
        list replaced the node's rows), ``delta`` (changes applied),
        ``unchanged`` (nothing new) or ``ignored`` (a delta against a
        generation this store does not hold; the next fetch asks for the
        full list)."""
        if payload.get("node_unchanged"):
            self._touch(name, vpn_ip, payload, now)
            return "unchanged"
        if "storage_indexes" in payload:
            fingerprint = (
                f"{payload.get('generation')}|{payload.get('computed_at')}|"
                f"{payload.get('object_count')}"
            )
            known = self._conn.execute(
                "SELECT fingerprint FROM nodes WHERE name = ?", (name,)
            ).fetchone()
            if known is not None and known[0] == fingerprint:
                self._touch(name, vpn_ip, payload, now)
                return "unchanged"
            with self._conn:
                self._conn.execute("DELETE FROM shares WHERE node = ?", (name,))
                self._conn.executemany(
                    "INSERT OR IGNORE INTO shares (si, node) VALUES (?, ?)",
                    ((str(si), name) for si in payload["storage_indexes"]),
                )
                self._upsert(name, vpn_ip, payload, now, payload.get("generation"), fingerprint)
            self._dirty = True
            return "full"
        if "added" in payload and "removed" in payload:
            held = self._conn.execute(
                "SELECT generation FROM nodes WHERE name = ?", (name,)
            ).fetchone()
            if held is None or held[0] is None or payload.get("base") != held[0]:
                with self._conn:
                    self._upsert(name, vpn_ip, payload, now, None, None)
                return "ignored"
            with self._conn:
                self._conn.executemany(
                    "DELETE FROM shares WHERE si = ? AND node = ?",
                    ((str(si), name) for si in payload["removed"]),
                )
                self._conn.executemany(
                    "INSERT OR IGNORE INTO shares (si, node) VALUES (?, ?)",
                    ((str(si), name) for si in payload["added"]),
                )
                self._upsert(name, vpn_ip, payload, now, payload.get("generation"), None)
            if payload["added"] or payload["removed"]:
                self._dirty = True
            return "delta"
        return "ignored"

    def mark_unreachable(self, name: str, now: float) -> float | None:
        """The node did not answer: keep its rows, flag it. Returns how old
        its inventory is, or None when it never reported."""
        row = self._conn.execute(
            "SELECT fetched_at, live FROM nodes WHERE name = ?", (name,)
        ).fetchone()
        if row is None:
            return None
        if row[1]:
            with self._conn:
                self._conn.execute("UPDATE nodes SET live = 0 WHERE name = ?", (name,))
            self._dirty = True
        return max(now - float(row[0]), 0.0)

    def set_live(self, name: str, live: bool) -> bool:
        """Count, or stop counting, a node's rows as reachable copies.
        Returns whether anything changed."""
        row = self._conn.execute("SELECT live FROM nodes WHERE name = ?", (name,)).fetchone()
        if row is None or bool(row[0]) == live:
            return False
        with self._conn:
            self._conn.execute("UPDATE nodes SET live = ? WHERE name = ?", (int(live), name))
        self._dirty = True
        return True

    def prune(self, keep: Iterable[str]) -> int:
        """Forget nodes that are no longer storage nodes of the manifest."""
        names = set(keep)
        stale = [
            row[0]
            for row in self._conn.execute("SELECT name FROM nodes").fetchall()
            if row[0] not in names
        ]
        orphans = [
            row[0]
            for row in self._conn.execute("SELECT DISTINCT node FROM shares").fetchall()
            if row[0] not in names
        ]
        if not stale and not orphans:
            return 0
        with self._conn:
            self._conn.executemany("DELETE FROM nodes WHERE name = ?", ((n,) for n in stale))
            self._conn.executemany(
                "DELETE FROM shares WHERE node = ?", ((n,) for n in set(stale) | set(orphans))
            )
        self._dirty = True
        return len(set(stale) | set(orphans))

    def import_legacy(self, cache_dir: Path, now: float) -> list[Path]:
        """Load the per-node JSON files an older hub wrote (one full census
        each, with ``cached_at``), as cached inventories. Returns the files
        imported, for the caller to remove."""
        imported: list[Path] = []
        for path in sorted(cache_dir.glob("*.json")) if cache_dir.is_dir() else []:
            try:
                payload = json.loads(path.read_text())
                indexes = payload["storage_indexes"]
                fetched_at = datetime.fromisoformat(payload["cached_at"]).timestamp()
            except (OSError, ValueError, KeyError, TypeError):
                continue
            name = str(payload.get("node") or path.stem)
            if not isinstance(indexes, list):
                continue
            self.ingest(name, "", payload, fetched_at)
            with self._conn:
                self._conn.execute("UPDATE nodes SET live = 0 WHERE name = ?", (name,))
            imported.append(path)
        if imported:
            self._dirty = True
        _ = now
        return imported

    # --- internals -------------------------------------------------------

    def _touch(self, name: str, vpn_ip: str, payload: dict[str, Any], now: float) -> None:
        with self._conn:
            if self._conn.execute("SELECT 1 FROM nodes WHERE name = ?", (name,)).fetchone():
                self._conn.execute(
                    "UPDATE nodes SET live = 1, fetched_at = ?, vpn_ip = COALESCE(NULLIF(?, ''),"
                    " vpn_ip), fetch_seconds = COALESCE(?, fetch_seconds) WHERE name = ?",
                    (now, vpn_ip, payload.get("fetch_seconds"), name),
                )
            else:
                self._upsert(name, vpn_ip, payload, now, payload.get("generation"), None)

    def _upsert(
        self,
        name: str,
        vpn_ip: str,
        payload: dict[str, Any],
        now: float,
        generation: Any,
        fingerprint: str | None,
    ) -> None:
        walk = payload.get("walk") or {}
        raw_total = payload.get("disk_total_bytes")
        raw_free = payload.get("disk_free_bytes")
        self._conn.execute(
            "INSERT INTO nodes (name, vpn_ip, generation, fingerprint, object_count,"
            " disk_used_bytes, disk_total_bytes, disk_free_bytes, computed_at, fetched_at, live,"
            " fetch_seconds, walk_seconds, walk_mode)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)"
            " ON CONFLICT(name) DO UPDATE SET vpn_ip = COALESCE(NULLIF(excluded.vpn_ip, ''),"
            " nodes.vpn_ip), generation = excluded.generation,"
            " fingerprint = COALESCE(excluded.fingerprint, nodes.fingerprint),"
            " object_count = excluded.object_count, disk_used_bytes = excluded.disk_used_bytes,"
            " disk_total_bytes = excluded.disk_total_bytes, disk_free_bytes = excluded.disk_free_bytes,"
            " computed_at = excluded.computed_at, fetched_at = excluded.fetched_at, live = 1,"
            " fetch_seconds = COALESCE(excluded.fetch_seconds, nodes.fetch_seconds),"
            " walk_seconds = COALESCE(excluded.walk_seconds, nodes.walk_seconds),"
            " walk_mode = COALESCE(excluded.walk_mode, nodes.walk_mode)",
            (
                name,
                vpn_ip or None,
                None if generation is None else int(generation),
                fingerprint,
                int(payload.get("object_count") or 0),
                int(payload.get("disk_used_bytes") or 0),
                None if raw_total is None else int(raw_total),
                None if raw_free is None else int(raw_free),
                payload.get("computed_at"),
                now,
                payload.get("fetch_seconds"),
                walk.get("seconds"),
                walk.get("mode"),
            ),
        )


def _record(row: tuple[Any, ...]) -> NodeRecord:
    return NodeRecord(
        name=str(row[0]),
        vpn_ip=row[1],
        generation=None if row[2] is None else int(row[2]),
        object_count=int(row[3]),
        disk_used_bytes=int(row[4]),
        disk_total_bytes=None if row[5] is None else int(row[5]),
        disk_free_bytes=None if row[6] is None else int(row[6]),
        computed_at=row[7],
        fetched_at=float(row[8]),
        live=bool(row[9]),
        fetch_seconds=None if row[10] is None else float(row[10]),
        walk_seconds=None if row[11] is None else float(row[11]),
        walk_mode=row[12],
    )
