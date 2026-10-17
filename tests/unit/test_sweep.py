"""Unit tests for the lease renewal and repair sweep (storage/sweep.py)."""

from __future__ import annotations

import threading
from pathlib import Path

import pytest

from redundanet.storage.dircache import DirCache
from redundanet.storage.sweep import (
    Outcome,
    SweepStore,
    Target,
    collect_targets,
    has_shares,
    parse_check,
    run_sweep,
    web_checker,
)


def chk(key: str, size: int = 100) -> str:
    return f"URI:CHK:{key}:hash:2:4:{size}"


ROOT = "URI:DIR2:root:x"
SNAP = "URI:DIR2-CHK:snap:x:2:4:9"


class FakeLister:
    def __init__(self, dirs: dict[str, dict]):
        self.dirs = dirs
        self.calls: list[str] = []

    def __call__(self, cap: str):
        self.calls.append(cap)
        if cap not in self.dirs:
            raise OSError(f"no directory {cap}")
        children = {
            name: [kind, {"ro_uri": child} if child else {}]
            for name, (kind, child) in self.dirs[cap].items()
        }
        return "dirnode", {"children": children}


def tree() -> FakeLister:
    return FakeLister(
        {
            ROOT: {
                "Latest": ("dirnode", SNAP),
                "Archives": ("dirnode", "URI:DIR2:arch:x"),
                "note.txt": ("filenode", "URI:LIT:tiny"),
            },
            "URI:DIR2:arch:x": {"2026-10-01": ("dirnode", SNAP), "2026-10-02": ("dirnode", SNAP)},
            SNAP: {"a": ("filenode", chk("a")), "b": ("filenode", chk("b"))},
        }
    )


class TestTargets:
    def test_directories_files_and_pending_each_once(self):
        targets = collect_targets(
            {"backups": ROOT}, tree(), pending_caps=[chk("a"), chk("new"), "URI:LIT:x"]
        )
        assert [t.kind for t in targets] == ["dir", "dir", "dir", "file", "file", "pending"]
        assert {t.cap for t in targets if t.kind == "dir"} == {ROOT, SNAP, "URI:DIR2:arch:x"}
        assert [t.cap for t in targets if t.kind != "dir"] == [chk("a"), chk("b"), chk("new")]

    def test_lit_caps_carry_no_shares(self):
        assert not has_shares("URI:LIT:abc") and not has_shares("URI:DIR2-LIT:abc")
        assert has_shares(chk("a")) and has_shares(ROOT) and has_shares(SNAP)

    def test_uses_the_directory_cache(self):
        cache = DirCache()
        collect_targets({"b": ROOT}, tree(), cache=cache)
        second = tree()
        targets = collect_targets({"b": ROOT}, second, cache=cache)
        assert SNAP not in second.calls
        assert any(t.cap == chk("a") for t in targets)


class TestParseCheck:
    def test_plain_check(self):
        outcome = parse_check({"results": {"healthy": True, "count-shares-good": 4}})
        assert outcome == Outcome(healthy=True, shares_good=4)
        assert parse_check({"results": {"healthy": False}}).healthy is False
        assert parse_check({}).healthy is False

    def test_check_and_repair(self):
        data = {
            "repair-attempted": True,
            "repair-successful": True,
            "pre-repair-results": {"results": {"healthy": False, "count-shares-good": 2}},
            "post-repair-results": {"results": {"healthy": True, "count-shares-good": 4}},
        }
        outcome = parse_check(data)
        assert outcome == Outcome(
            healthy=True, repair_attempted=True, repair_successful=True, shares_good=4
        )
        data["repair-successful"] = False
        data["post-repair-results"] = {"results": {"healthy": False, "count-shares-good": 2}}
        assert parse_check(data).healthy is False

    def test_checker_accepts_only_http(self):
        assert callable(web_checker("http://127.0.0.1:3456/"))
        with pytest.raises(ValueError, match="http"):
            web_checker("file:///x")


class TestStore:
    def test_sweep_lifecycle_and_resume(self, tmp_path: Path):
        store = SweepStore(tmp_path / "sweep.sqlite")
        assert store.current() is None and store.last_finished_at() is None
        sweep_id, _started = store.start(100.0)
        assert store.current() == (sweep_id, 100.0)
        store.record(Target(chk("a"), "file"), Outcome(healthy=True), 101.0)
        store.record(Target(chk("old"), "file"), Outcome(healthy=True), 50.0)  # an older sweep
        assert store.done_since(100.0) == {chk("a")}
        store.close()
        again = SweepStore(tmp_path / "sweep.sqlite")  # a restart
        assert again.current() == (sweep_id, 100.0)
        stats = run_sweep([], lambda _cap: {}, again, 100.0)
        again.finish(sweep_id, 200.0, stats)
        assert again.current() is None and again.last_finished_at() == 200.0
        assert again.forget(before=100.0) == 1


class Checked:
    """A checker that answers healthy, records calls, and can fail or
    report some objects unhealthy."""

    def __init__(self, failing=(), unhealthy=(), repaired=()):
        self.failing, self.unhealthy, self.repaired = set(failing), set(unhealthy), set(repaired)
        self.calls: list[str] = []
        self._lock = threading.Lock()

    def __call__(self, cap: str) -> dict:
        with self._lock:
            self.calls.append(cap)
        if cap in self.failing:
            raise OSError("node unreachable")
        if cap in self.repaired:
            return {
                "repair-attempted": True,
                "repair-successful": True,
                "pre-repair-results": {"results": {"healthy": False}},
                "post-repair-results": {"results": {"healthy": True}},
            }
        return {"results": {"healthy": cap not in self.unhealthy}}


def targets(n: int) -> list[Target]:
    return [Target(chk(f"k{i}"), "file") for i in range(n)]


class TestRunSweep:
    def test_checks_everything_and_reports(self):
        store = SweepStore()
        check = Checked(failing=[chk("k1")], unhealthy=[chk("k2")], repaired=[chk("k3")])
        healthy_batches: list[list[Target]] = []
        stats = run_sweep(
            targets(5), check, store, 0.0, workers=1, on_healthy=healthy_batches.append
        )
        assert (stats.checked, stats.healthy, stats.unhealthy, stats.repaired, stats.failed) == (
            4,
            3,
            1,
            1,
            1,
        )
        assert stats.unhealthy_caps == [chk("k2")] and not stats.complete
        assert sorted(t.cap for b in healthy_batches for t in b) == sorted(
            [chk("k0"), chk("k3"), chk("k4")]
        )
        assert store.done_since(0.0) == {chk("k0"), chk("k2"), chk("k3"), chk("k4")}

    def test_resume_skips_what_this_sweep_already_did(self):
        store = SweepStore()
        first = Checked()
        run_sweep(targets(3), first, store, 10.0, workers=1)
        second = Checked()
        stats = run_sweep(targets(5), second, store, 10.0, workers=1)
        assert stats.already_done == 3 and stats.checked == 2
        assert sorted(second.calls) == [chk("k3"), chk("k4")]

    def test_budget_stops_between_batches(self):
        ticks = iter([0.0, 0.0, 500.0, 500.0])
        stats = run_sweep(
            targets(10),
            Checked(),
            SweepStore(),
            0.0,
            workers=1,
            budget=100,
            clock=lambda: next(ticks),
        )
        assert stats.budget_stop and stats.checked == 4  # one batch of workers*4
        assert "resuming later" in stats.summary()

    def test_parallel_matches_sequential(self):
        many = targets(50)
        sequential = run_sweep(many, Checked(), SweepStore(), 0.0, workers=1)
        parallel_check = Checked()
        parallel = run_sweep(many, parallel_check, SweepStore(), 0.0, workers=4)
        assert (parallel.checked, parallel.healthy) == (sequential.checked, sequential.healthy)
        assert len(parallel_check.calls) == 50 and len(set(parallel_check.calls)) == 50
