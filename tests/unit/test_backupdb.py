"""Unit tests for redundanet.storage.backupdb (pruning tahoe backup's database)."""

from __future__ import annotations

import sqlite3
from pathlib import Path

from redundanet.storage.backupdb import Pruned, prune_stale, recorded_caps

# Tahoe-LAFS 1.20 backupdb schema, version 2 (allmydata/scripts/backupdb.py).
SCHEMA_V2 = """
CREATE TABLE version (version INTEGER);
CREATE TABLE local_files (
  path VARCHAR(1024) PRIMARY KEY, size INTEGER, mtime NUMBER, ctime NUMBER, fileid INTEGER
);
CREATE TABLE caps (fileid INTEGER PRIMARY KEY AUTOINCREMENT, filecap VARCHAR(256) UNIQUE);
CREATE TABLE last_upload (fileid INTEGER PRIMARY KEY, last_uploaded TIMESTAMP, last_checked TIMESTAMP);
CREATE TABLE directories (
  dirhash varchar(256) PRIMARY KEY, dircap varchar(256), last_uploaded TIMESTAMP, last_checked TIMESTAMP
);
INSERT INTO version VALUES (2);
"""


def chk(k: int, n: int, key: str) -> bytes:
    # Tahoe stores caps as bytes.
    return f"URI:CHK:{key}:hash:{k}:{n}:1000".encode()


def dircap(k: int, n: int, key: str) -> bytes:
    return f"URI:DIR2-CHK:{key}:hash:{k}:{n}:500".encode()


def make_db(path: Path, files: dict[str, bytes], dirs: dict[str, bytes], schema=SCHEMA_V2):
    conn = sqlite3.connect(path)
    conn.executescript(schema)
    for i, (local_path, cap) in enumerate(files.items(), start=1):
        conn.execute("INSERT INTO caps (fileid, filecap) VALUES (?, ?)", (i, cap))
        conn.execute(
            "INSERT INTO local_files VALUES (?, ?, ?, ?, ?)", (local_path, 10, 1.0, 1.0, i)
        )
        conn.execute("INSERT INTO last_upload VALUES (?, ?, ?)", (i, 1.0, 1.0))
    for dirhash, cap in dirs.items():
        conn.execute("INSERT INTO directories VALUES (?, ?, ?, ?)", (dirhash, cap, 1.0, 1.0))
    conn.commit()
    conn.close()


def rows(path: Path, table: str, column: str) -> set[bytes]:
    conn = sqlite3.connect(path)
    try:
        return {r[0] for r in conn.execute(f"SELECT {column} FROM {table}")}  # noqa: S608
    finally:
        conn.close()


class TestPruneStale:
    def test_only_rows_at_another_encoding_are_dropped(self, tmp_path: Path):
        db = tmp_path / "backupdb.sqlite"
        make_db(
            db,
            files={
                "/data/sync/old.bin": chk(1, 2, "old"),
                "/data/sync/new.bin": chk(2, 4, "new"),
                "/data/sync/tiny.txt": b"URI:LIT:abcd",  # no shares, never stale
            },
            dirs={"h-old": dircap(1, 2, "d1"), "h-new": dircap(2, 4, "d2")},
        )
        pruned = prune_stale(db, 2, 4)
        assert pruned == Pruned(files=1, directories=1)
        assert bool(pruned) is True
        assert rows(db, "caps", "filecap") == {chk(2, 4, "new"), b"URI:LIT:abcd"}
        # every table keyed by the fileid is cleaned, not just caps
        assert rows(db, "local_files", "path") == {"/data/sync/new.bin", "/data/sync/tiny.txt"}
        assert rows(db, "last_upload", "fileid") == {2, 3}
        assert rows(db, "directories", "dirhash") == {"h-new"}

    def test_nothing_to_do_when_everything_matches(self, tmp_path: Path):
        db = tmp_path / "backupdb.sqlite"
        make_db(db, files={"/a": chk(2, 4, "a")}, dirs={"h": dircap(2, 4, "d")})
        pruned = prune_stale(db, 2, 4)
        assert pruned == Pruned(0, 0)
        assert bool(pruned) is False
        assert rows(db, "caps", "filecap") == {chk(2, 4, "a")}

    def test_missing_database_is_none(self, tmp_path: Path):
        """A node that never backed up has no database; nothing to prune."""
        assert prune_stale(tmp_path / "absent.sqlite", 2, 4) is None

    def test_schema_v1_without_directories_table(self, tmp_path: Path):
        db = tmp_path / "backupdb.sqlite"
        v1 = SCHEMA_V2.replace(
            "CREATE TABLE directories (\n  dirhash varchar(256) PRIMARY KEY, dircap varchar(256),"
            " last_uploaded TIMESTAMP, last_checked TIMESTAMP\n);\n",
            "",
        ).replace("INSERT INTO version VALUES (2);", "INSERT INTO version VALUES (1);")
        make_db(db, files={"/a": chk(1, 2, "a")}, dirs={}, schema=v1)
        assert prune_stale(db, 2, 4) == Pruned(files=1, directories=0)
        assert rows(db, "caps", "filecap") == set()

    def test_text_caps_are_handled_too(self, tmp_path: Path):
        db = tmp_path / "backupdb.sqlite"
        make_db(db, files={}, dirs={})
        conn = sqlite3.connect(db)
        conn.execute("INSERT INTO caps (fileid, filecap) VALUES (1, ?)", ("URI:CHK:k:h:1:2:9",))
        conn.commit()
        conn.close()
        assert prune_stale(db, 2, 4) == Pruned(files=1, directories=0)


class TestRecordedCaps:
    def test_every_uploaded_cap_linked_or_not(self, tmp_path: Path):
        db = tmp_path / "backupdb.sqlite"
        make_db(
            db, files={"/a": chk(2, 4, "a"), "/b": chk(2, 4, "b"), "/t": b"URI:LIT:abcd"}, dirs={}
        )
        assert sorted(recorded_caps(db)) == sorted(
            ["URI:CHK:a:hash:2:4:1000", "URI:CHK:b:hash:2:4:1000", "URI:LIT:abcd"]
        )

    def test_missing_or_broken_database_is_empty(self, tmp_path: Path):
        assert recorded_caps(tmp_path / "absent.sqlite") == []
        (tmp_path / "bad.sqlite").write_bytes(b"not a database")
        assert recorded_caps(tmp_path / "bad.sqlite") == []
