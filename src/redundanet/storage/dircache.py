"""The listings of immutable directories, kept between walks.

``tahoe backup`` makes every snapshot an immutable directory tree, and a
directory whose contents did not change keeps its capability from one
snapshot to the next. Listing a directory costs a round trip to the grid
whatever its size, and a member's archive has thousands of them, so the
usage meter remembers what each immutable directory contains and lists
only the directories it has never seen: after the first measurement, a
run costs what the latest snapshot added.

Mutable directories are never cached: their owner rewrites them.

SQLite on the client node's volume, used from one thread at a time.
"""

from __future__ import annotations

import sqlite3
import time
from pathlib import Path

from redundanet.storage.inventory import Child, DirectoryCache

SCHEMA = """
CREATE TABLE IF NOT EXISTS dirs (
    cap TEXT PRIMARY KEY,
    listed_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS children (
    dircap TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    cap TEXT NOT NULL,
    PRIMARY KEY (dircap, name)
) WITHOUT ROWID;
"""


class DirCache(DirectoryCache):
    def __init__(self, path: Path | str = ":memory:") -> None:
        self._conn = sqlite3.connect(str(path), timeout=30)
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA synchronous=NORMAL")
        self._conn.executescript(SCHEMA)

    def close(self) -> None:
        self._conn.close()

    def get(self, cap: str) -> list[Child] | None:
        if self._conn.execute("SELECT 1 FROM dirs WHERE cap = ?", (cap,)).fetchone() is None:
            return None
        rows = self._conn.execute(
            "SELECT name, kind, cap FROM children WHERE dircap = ? ORDER BY name", (cap,)
        ).fetchall()
        return [(str(name), str(kind), str(child)) for name, kind, child in rows]

    def put(self, cap: str, children: list[Child], now: float | None = None) -> None:
        now = time.time() if now is None else now
        with self._conn:
            self._conn.execute("DELETE FROM children WHERE dircap = ?", (cap,))
            self._conn.executemany(
                "INSERT INTO children (dircap, name, kind, cap) VALUES (?, ?, ?, ?)",
                ((cap, name, kind, child) for name, kind, child in children),
            )
            self._conn.execute(
                "INSERT INTO dirs (cap, listed_at) VALUES (?, ?)"
                " ON CONFLICT(cap) DO UPDATE SET listed_at = excluded.listed_at",
                (cap, now),
            )

    def size(self) -> int:
        """How many directories are cached."""
        return int(self._conn.execute("SELECT count(*) FROM dirs").fetchone()[0])
