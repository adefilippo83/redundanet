"""Unit tests for the usage meter sidecar (docker/entrypoints/usage_report.py)."""

from __future__ import annotations

import sqlite3
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import usage_report  # noqa: E402

from redundanet.storage.dircache import DirCache  # noqa: E402


def chk(key: str, size: int = 1000, k: int = 2, n: int = 4) -> str:
    return f"URI:CHK:{key}:hash:{k}:{n}:{size}"


ROOT = "URI:DIR2:x:y"


class FakeRun:
    """tahoe list-aliases: one alias."""

    def __call__(self, args, timeout=3600):
        if args[0] == "list-aliases":
            return subprocess.CompletedProcess(args, 0, f"backups: {ROOT}\n", "")
        return subprocess.CompletedProcess(args, 1, "", "unexpected")


def lister(linked: dict[str, str | None], failing: tuple[str, ...] = ()):
    """The node's web API: the alias root holds the given files; a None cap
    is a subdirectory (immutable) with nothing of interest."""

    def list_dir(cap: str):
        if cap in failing:
            raise TimeoutError("timed out")
        if cap != ROOT:
            return "dirnode", {"children": {}}
        children = {
            name: (
                ["dirnode", {"ro_uri": f"URI:DIR2-CHK:{name}:x:2:4:9"}]
                if child is None
                else ["filenode", {"ro_uri": child, "size": 1}]
            )
            for name, child in linked.items()
        }
        return "dirnode", {"children": children}

    return list_dir


def backupdb(path: Path, caps: list[str]) -> Path:
    conn = sqlite3.connect(path)
    conn.executescript("CREATE TABLE caps (fileid INTEGER PRIMARY KEY, filecap UNIQUE);")
    conn.executemany(
        "INSERT INTO caps VALUES (?, ?)", [(i, c.encode()) for i, c in enumerate(caps, 1)]
    )
    conn.commit()
    conn.close()
    return path


class TestMeasure:
    def test_counts_unlinked_uploads_from_the_backupdb(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        db = backupdb(
            tmp_path / "backupdb.sqlite", [chk("linked"), chk("pending1"), chk("pending2")]
        )
        payload = usage_report.measure(
            "n1",
            {},
            run=FakeRun(),
            list_dir=lister({"a.bin": chk("linked")}),
            cache=DirCache(),
            backupdb=db,
        )
        assert payload["files"] == 3  # everything on the grid
        assert payload["in_progress_files"] == 2  # not yet in any snapshot
        assert payload["used_bytes"] == 3 * 2000  # 1000 bytes at 2-of-4 each
        assert payload["in_progress_bytes"] == 2 * 2000

    def test_completed_backup_has_nothing_in_progress(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        db = backupdb(tmp_path / "backupdb.sqlite", [chk("a")])
        payload = usage_report.measure(
            "n1",
            {},
            run=FakeRun(),
            list_dir=lister({"a.bin": chk("a")}),
            cache=DirCache(),
            backupdb=db,
        )
        assert payload["files"] == 1 and payload["in_progress_files"] == 0

    def test_no_backupdb_at_all(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        payload = usage_report.measure(
            "n1",
            {},
            run=FakeRun(),
            list_dir=lister({"a.bin": chk("a")}),
            cache=DirCache(),
            backupdb=tmp_path / "none",
        )
        assert payload["files"] == 1 and payload["in_progress_files"] == 0


class TestMeasureDue:
    def test_interval_and_backup_marker(self, tmp_path: Path):
        assert usage_report.measure_due(0.0, 3600, 0.0, now=10_000.0) is True  # never measured
        assert usage_report.measure_due(1000.0, 3600, 0.0, now=1500.0) is False
        assert usage_report.measure_due(1000.0, 3600, 0.0, now=4600.0) is True
        # a backup that made a snapshot after the last measurement triggers one
        assert usage_report.measure_due(1000.0, 3600, 1200.0, now=1500.0) is True
        assert usage_report.measure_due(1300.0, 3600, 1200.0, now=1500.0) is False

    def test_backup_marker_mtime(self, tmp_path: Path):
        marker = tmp_path / "done"
        assert usage_report.backup_done_at(marker) == 0.0
        marker.touch()
        assert usage_report.backup_done_at(marker) > 0.0


class TestPartialAndStartup:
    def test_partial_measurement_when_a_listing_times_out(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        list_dir = lister({"a.bin": chk("a"), "big": None}, failing=("URI:DIR2-CHK:big:x:2:4:9",))
        payload = usage_report.measure(
            "n1", {}, run=FakeRun(), backupdb=tmp_path / "none", list_dir=list_dir, cache=DirCache()
        )
        assert payload["files"] == 1
        assert payload["partial"] is True and payload["skipped_dirs"] == 1

    def test_complete_measurement_is_not_partial(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        payload = usage_report.measure(
            "n1",
            {},
            run=FakeRun(),
            list_dir=lister({"a.bin": chk("a")}),
            cache=DirCache(),
            backupdb=tmp_path / "none",
        )
        assert payload["partial"] is False and payload["skipped_dirs"] == 0

    def test_last_report_is_served_until_the_first_measurement(self, tmp_path: Path):
        """A restart must not blank /usage: the previous life's report stands
        (with its own computed_at) until a new measurement completes."""
        import json

        path = tmp_path / "usage.json"
        assert usage_report.initial_report(path) == {}
        path.write_text(
            json.dumps({"node": "n1", "used_bytes": 5, "computed_at": "2026-09-14T17:15:36+00:00"})
        )
        assert usage_report.initial_report(path)["computed_at"] == "2026-09-14T17:15:36+00:00"
        path.write_text("garbage")
        assert usage_report.initial_report(path) == {}


class TestWalkSettings:
    def test_workers_from_the_environment(self):
        assert usage_report.walk_workers({}) == usage_report.DEFAULT_WORKERS
        assert usage_report.walk_workers({"REDUNDANET_WALK_WORKERS": "8"}) == 8
        assert usage_report.walk_workers({"REDUNDANET_WALK_WORKERS": "0"}) == 1
        assert usage_report.walk_workers({"REDUNDANET_WALK_WORKERS": "x"}) == 4

    def test_dircache_falls_back_to_memory(self, tmp_path: Path):
        cache = usage_report.open_dircache(tmp_path / "missing-dir" / "cache.sqlite")
        assert cache.size() == 0
        on_disk = usage_report.open_dircache(tmp_path / "cache.sqlite")
        on_disk.put("URI:DIR2-CHK:a:b:1:2:3", [])
        assert usage_report.open_dircache(tmp_path / "cache.sqlite").size() == 1
