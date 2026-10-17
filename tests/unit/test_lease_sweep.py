"""Unit tests for the lease sweep sidecar (docker/entrypoints/lease_sweep.py)."""

from __future__ import annotations

import sqlite3
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import lease_sweep  # noqa: E402

from redundanet.storage.dircache import DirCache  # noqa: E402
from redundanet.storage.sweep import SweepStore  # noqa: E402

ROOT = "URI:DIR2:root:x"


def chk(key: str) -> str:
    return f"URI:CHK:{key}:hash:2:4:100"


def run(args, timeout=3600):
    if args[0] == "list-aliases":
        return subprocess.CompletedProcess(args, 0, f"backups: {ROOT}\n", "")
    return subprocess.CompletedProcess(args, 1, "", "unexpected")


def list_dir(cap: str):
    if cap == ROOT:
        return "dirnode", {
            "children": {
                "a": ["filenode", {"ro_uri": chk("a")}],
                "b": ["filenode", {"ro_uri": chk("b")}],
            }
        }
    raise OSError("no such directory")


def backupdb(path: Path, caps: list[str], last_checked: float = 1.0) -> Path:
    conn = sqlite3.connect(path)
    conn.executescript(
        "CREATE TABLE caps (fileid INTEGER PRIMARY KEY, filecap UNIQUE);"
        "CREATE TABLE last_upload (fileid INTEGER PRIMARY KEY, last_uploaded, last_checked);"
        "CREATE TABLE directories (dirhash PRIMARY KEY, dircap, last_uploaded, last_checked);"
    )
    for i, cap in enumerate(caps, 1):
        conn.execute("INSERT INTO caps VALUES (?, ?)", (i, cap.encode()))
        conn.execute("INSERT INTO last_upload VALUES (?, ?, ?)", (i, 1.0, last_checked))
    conn.commit()
    conn.close()
    return path


def last_checked(path: Path) -> dict[str, float]:
    conn = sqlite3.connect(path)
    try:
        return {
            cap.decode(): checked
            for cap, checked in conn.execute(
                "SELECT filecap, last_checked FROM caps JOIN last_upload USING (fileid)"
            )
        }
    finally:
        conn.close()


class TestConfig:
    def test_defaults_and_overrides(self):
        config = lease_sweep.parse_config({})
        assert (config.interval, config.timeout, config.repair, config.workers) == (
            604800,
            21600,
            True,
            4,
        )
        config = lease_sweep.parse_config(
            {
                "REDUNDANET_LEASE_RENEW_INTERVAL": "60",
                "REDUNDANET_LEASE_RENEW_TIMEOUT": "x",
                "REDUNDANET_REPAIR_ENABLED": "false",
                "REDUNDANET_SWEEP_WORKERS": "0",
            }
        )
        assert (config.interval, config.timeout, config.repair, config.workers) == (
            60,
            21600,
            False,
            1,
        )


class TestSchedule:
    def test_due_from_the_store_or_the_legacy_file(self, tmp_path: Path):
        store = SweepStore()
        legacy = tmp_path / "last-run"
        assert lease_sweep.seconds_until_due(store, 100, now=1000.0, legacy=legacy) == 0.0
        legacy.write_text("950\n")
        assert lease_sweep.seconds_until_due(store, 100, now=1000.0, legacy=legacy) == 50.0
        sweep_id, _ = store.start(990.0)
        store.finish(sweep_id, 995.0, lease_sweep.SweepStats())
        assert lease_sweep.seconds_until_due(store, 100, now=1000.0, legacy=legacy) == 95.0
        legacy.write_text("garbage")
        assert lease_sweep.seconds_until_due(SweepStore(), 100, now=1000.0, legacy=legacy) == 0.0


class TestSweepOnce:
    def config(self, **overrides) -> lease_sweep.SweepConfig:
        return lease_sweep.SweepConfig(
            **{"interval": 60, "timeout": 3600, "repair": True, "workers": 1, **overrides}
        )

    def test_renews_files_directories_and_pending_uploads(self, tmp_path: Path):
        db = backupdb(tmp_path / "backupdb.sqlite", [chk("a"), chk("pending")])
        checked: list[str] = []

        def check(cap: str) -> dict:
            checked.append(cap)
            return {"results": {"healthy": True}}

        store = SweepStore()
        stats = lease_sweep.sweep_once(
            self.config(),
            store,
            run=run,
            list_dir=list_dir,
            check=check,
            cache=DirCache(),
            backupdb=db,
            now=lambda: 5000.0,
            legacy=tmp_path / "last-run",
        )
        assert sorted(checked) == sorted([ROOT, chk("a"), chk("b"), chk("pending")])
        assert stats.complete and stats.checked == 4
        assert store.current() is None and store.last_finished_at() == 5000.0
        assert (tmp_path / "last-run").read_text().strip() == "5000"
        # tahoe backup will not re-check what the sweep just verified
        assert last_checked(db) == {chk("a"): 5000.0, chk("pending"): 5000.0}

    def test_budget_pause_leaves_the_sweep_open_to_resume(self, tmp_path: Path):
        def many(cap: str):  # root + 6 files: two batches of four at one worker
            if cap == ROOT:
                children = {f"f{i}": ["filenode", {"ro_uri": chk(f"f{i}")}] for i in range(6)}
                return "dirnode", {"children": children}
            raise OSError("no such directory")

        ticks = iter([0.0, 0.0, 10.0, 10.0, 10.0, 10.0])
        store = SweepStore()
        stats = lease_sweep.sweep_once(
            self.config(timeout=5),
            store,
            run=run,
            list_dir=many,
            check=lambda _cap: {"results": {"healthy": True}},
            cache=DirCache(),
            backupdb=tmp_path / "none",
            clock=lambda: next(ticks),
            legacy=tmp_path / "last-run",
        )
        assert stats.budget_stop and stats.checked == 4 and store.current() is not None
        assert not (tmp_path / "last-run").exists()
        resumed = lease_sweep.sweep_once(
            self.config(),
            store,
            run=run,
            list_dir=many,
            check=lambda _cap: {"results": {"healthy": True}},
            cache=DirCache(),
            backupdb=tmp_path / "none",
            legacy=tmp_path / "last-run",
        )
        assert resumed.complete and resumed.already_done == stats.checked
        assert store.current() is None

    def test_locked_backupdb_does_not_stop_the_sweep(self, tmp_path: Path, monkeypatch):
        def locked(*_args, **_kwargs):
            raise sqlite3.OperationalError("database is locked")

        monkeypatch.setattr(lease_sweep, "mark_checked", locked)
        stats = lease_sweep.sweep_once(
            self.config(),
            SweepStore(),
            run=run,
            list_dir=list_dir,
            check=lambda _cap: {"results": {"healthy": True}},
            cache=DirCache(),
            backupdb=tmp_path / "none",
            legacy=tmp_path / "last-run",
        )
        assert stats.complete and stats.checked == 3


class TestStartup:
    def test_waits_until_the_node_exists_before_touching_the_volume(self, tmp_path: Path):
        """tahoe create-client refuses a non-empty node directory: the sweep
        must not create its database there before the node is created."""
        node_url = tmp_path / "node.url"
        waited: list[float] = []

        def sleep(seconds: float) -> None:
            waited.append(seconds)
            if len(waited) == 3:
                node_url.write_text("http://127.0.0.1:3456/\n")

        lease_sweep.wait_for_node(node_url, sleep=sleep, poll=5)
        assert waited == [5, 5, 5]
        assert list(tmp_path.iterdir()) == [node_url]  # nothing else was written
