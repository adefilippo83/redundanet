"""``tahoe backup``'s local database: forget entries made at an old encoding.

``tahoe backup`` keeps ``private/backupdb.sqlite`` in the client node. A file
whose size and mtime match its row is not uploaded again: the capability
recorded there is linked into the new snapshot as is, and an unchanged
directory reuses its recorded capability the same way. Capabilities carry
their k-of-n, so after an encoding change every unchanged file keeps the old
encoding in every new snapshot, forever; the rebalancer cannot help because
snapshots are immutable directories. Deleting exactly the rows whose
capability differs from the node's current encoding makes the next run
re-upload those files (and rebuild the directories containing them) at the
new parameters, and nothing else. Tahoe recreates the rows itself.

Schema (Tahoe-LAFS 1.20, backupdb version 2)::

    local_files(path PRIMARY KEY, size, mtime, ctime, fileid)
    caps(fileid PRIMARY KEY, filecap UNIQUE)            -- URI:CHK:...
    last_upload(fileid PRIMARY KEY, last_uploaded, last_checked)
    directories(dirhash PRIMARY KEY, dircap, ...)       -- URI:DIR2-CHK:...

The ``directories`` table only exists from schema version 2 on; older files
are pruned by file only.
"""

from __future__ import annotations

import sqlite3
from dataclasses import dataclass
from pathlib import Path

from redundanet.storage.inventory import parse_encoding

BACKUPDB_FILE = Path("/var/lib/tahoe-client/private/backupdb.sqlite")


@dataclass(frozen=True)
class Pruned:
    files: int
    directories: int

    def __bool__(self) -> bool:
        return bool(self.files or self.directories)


def _stale(cap: str | bytes, encoding: tuple[int, int]) -> bool:
    """A cap made at another k-of-n. LIT and unparsable caps are never stale."""
    text = cap.decode("ascii", "replace") if isinstance(cap, bytes) else cap
    params = parse_encoding(text)
    return params is not None and params != encoding


def prune_stale(db_path: Path, needed: int, total: int) -> Pruned | None:
    """Delete the rows recorded at an encoding other than ``needed``-of-``total``.

    Returns what was dropped, or None when there is no database yet (a node
    that never backed up). Runs in one transaction, so a crash leaves the
    database either untouched or fully pruned.
    """
    if not db_path.is_file():
        return None
    encoding = (needed, total)
    conn = sqlite3.connect(db_path, timeout=30)
    try:
        conn.execute("BEGIN IMMEDIATE")
        stale_files = [
            (fileid,)
            for fileid, cap in conn.execute("SELECT fileid, filecap FROM caps")
            if _stale(cap, encoding)
        ]
        conn.executemany("DELETE FROM local_files WHERE fileid = ?", stale_files)
        conn.executemany("DELETE FROM last_upload WHERE fileid = ?", stale_files)
        conn.executemany("DELETE FROM caps WHERE fileid = ?", stale_files)
        stale_dirs: list[tuple[str, ...]] = []
        try:
            stale_dirs = [
                (dirhash,)
                for dirhash, cap in conn.execute("SELECT dirhash, dircap FROM directories")
                if _stale(cap, encoding)
            ]
            conn.executemany("DELETE FROM directories WHERE dirhash = ?", stale_dirs)
        except sqlite3.OperationalError:  # schema v1: no directories table
            stale_dirs = []
        conn.commit()
    finally:
        conn.close()
    return Pruned(files=len(stale_files), directories=len(stale_dirs))
