"""Unit tests for the usage meter sidecar (docker/entrypoints/usage_report.py)."""

from __future__ import annotations

import sqlite3
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import usage_report  # noqa: E402


def chk(key: str, size: int = 1000, k: int = 2, n: int = 4) -> str:
    return f"URI:CHK:{key}:hash:{k}:{n}:{size}"


def dirnode_json(children: dict[str, str]) -> str:
    import json

    return json.dumps(
        [
            "dirnode",
            {
                "children": {
                    name: ["filenode", {"ro_uri": cap, "size": 1}] for name, cap in children.items()
                }
            },
        ]
    )


class FakeRun:
    """tahoe list-aliases + ls --json for one alias holding the given files."""

    def __init__(self, linked: dict[str, str]):
        self.linked = linked

    def __call__(self, args, timeout=3600):
        if args[0] == "list-aliases":
            return subprocess.CompletedProcess(args, 0, "backups: URI:DIR2:x:y\n", "")
        if args[0] == "ls":
            return subprocess.CompletedProcess(args, 0, dirnode_json(self.linked), "")
        return subprocess.CompletedProcess(args, 1, "", "unexpected")


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
        payload = usage_report.measure("n1", {}, run=FakeRun({"a.bin": chk("linked")}), backupdb=db)
        assert payload["files"] == 3  # everything on the grid
        assert payload["in_progress_files"] == 2  # not yet in any snapshot
        assert payload["used_bytes"] == 3 * 2000  # 1000 bytes at 2-of-4 each
        assert payload["in_progress_bytes"] == 2 * 2000

    def test_completed_backup_has_nothing_in_progress(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        db = backupdb(tmp_path / "backupdb.sqlite", [chk("a")])
        payload = usage_report.measure("n1", {}, run=FakeRun({"a.bin": chk("a")}), backupdb=db)
        assert payload["files"] == 1 and payload["in_progress_files"] == 0

    def test_no_backupdb_at_all(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        payload = usage_report.measure(
            "n1", {}, run=FakeRun({"a.bin": chk("a")}), backupdb=tmp_path / "none"
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

        class SlowRun(FakeRun):
            def __call__(self, args, timeout=3600):
                if args[0] == "ls" and args[-1] == "backups:big":
                    raise subprocess.TimeoutExpired(args, timeout)
                return super().__call__(args, timeout)

        run = SlowRun({"a.bin": chk("a")})
        run.linked = {"a.bin": chk("a"), "big": None}

        def dir_json(children):
            import json

            return json.dumps(
                [
                    "dirnode",
                    {
                        "children": {
                            name: (
                                ["dirnode", {"ro_uri": "URI:DIR2-CHK:big:x:2:4:9"}]
                                if cap is None
                                else ["filenode", {"ro_uri": cap, "size": 1}]
                            )
                            for name, cap in children.items()
                        }
                    },
                ]
            )

        def call(args, timeout=3600):
            if args[0] == "list-aliases":
                return subprocess.CompletedProcess(args, 0, "backups: URI:DIR2:x:y\n", "")
            if args[0] == "ls" and args[-1] == "backups:big":
                raise subprocess.TimeoutExpired(args, timeout)
            if args[0] == "ls":
                return subprocess.CompletedProcess(args, 0, dir_json(run.linked), "")
            return subprocess.CompletedProcess(args, 1, "", "unexpected")

        payload = usage_report.measure("n1", {}, run=call, backupdb=tmp_path / "none")
        assert payload["files"] == 1
        assert payload["partial"] is True and payload["skipped_dirs"] == 1

    def test_complete_measurement_is_not_partial(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr(usage_report, "load_manifest", lambda: {})
        monkeypatch.setattr(usage_report, "USAGE_FILE", tmp_path / "usage.json")
        payload = usage_report.measure(
            "n1", {}, run=FakeRun({"a.bin": chk("a")}), backupdb=tmp_path / "none"
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
