"""Unit tests for the alias walker / grid footprint (storage/inventory.py),
the directory cache (storage/dircache.py) and the usage reports
(monitor/usage.py)."""

from __future__ import annotations

import subprocess
import threading
from datetime import UTC, datetime
from pathlib import Path

from redundanet.monitor.usage import (
    load_usage_file,
    over_allocation,
    usage_payload,
    write_usage_file,
)
from redundanet.storage.dircache import DirCache
from redundanet.storage.inventory import (
    Footprint,
    alias_caps,
    all_file_caps,
    grid_footprint,
    list_aliases,
    parse_chk,
    parse_chk_params,
    parse_encoding,
    read_node_url,
    walk_tree,
)


def chk(k: int, n: int, size: int, key: str = "aaaa") -> str:
    return f"URI:CHK:{key}:hash:{k}:{n}:{size}"


def completed(rc: int = 0, stdout: str = "", stderr: str = ""):
    return subprocess.CompletedProcess(args=["tahoe"], returncode=rc, stdout=stdout, stderr=stderr)


class FakeLister:
    """The node's web API: directory capability -> {name: (kind, cap)}."""

    def __init__(self, dirs: dict[str, dict], failing: tuple[str, ...] = ()):
        self.dirs = dirs
        self.failing = set(failing)
        self.calls: list[str] = []
        self._lock = threading.Lock()

    def __call__(self, cap: str):
        with self._lock:
            self.calls.append(cap)
        if cap in self.failing:
            raise TimeoutError(f"listing {cap} timed out")
        if cap not in self.dirs:
            raise OSError(f"no directory {cap}")
        children = {
            name: [kind, {"ro_uri": child} if child else {}]
            for name, (kind, child) in self.dirs[cap].items()
        }
        return "dirnode", {"children": children}


class TestParse:
    def test_chk_fields(self):
        params = parse_chk(chk(2, 4, 1000))
        assert params is not None
        assert (params.needed, params.total, params.size) == (2, 4, 1000)
        assert parse_chk_params(chk(3, 10, 5)) == (3, 10)

    def test_non_chk(self):
        for cap in ("URI:LIT:abcd", "URI:DIR2:a:b", "URI:SSK:a:b", "garbage", "URI:CHK:a:b:x:2:3"):
            assert parse_chk(cap) is None


class TestFootprint:
    def test_each_file_weighted_by_its_own_encoding(self):
        footprint = grid_footprint(
            [chk(1, 2, 100), chk(2, 4, 100, "b"), chk(3, 10, 100, "c"), "URI:LIT:x", "URI:DIR2:a:b"]
        )
        assert footprint == Footprint(used_bytes=200 + 200 + 334, data_bytes=300, files=3)

    def test_empty(self):
        assert grid_footprint([]) == Footprint(0, 0, 0)

    def test_footprint_counts_each_capability_once(self):
        footprint = grid_footprint([chk(1, 2, 100)] * 50 + [chk(1, 2, 100, "other")])
        assert footprint == Footprint(used_bytes=400, data_bytes=200, files=2)


class TestAliases:
    def test_names_and_caps_from_list_aliases(self):
        def run(args, timeout=60):
            return completed(stdout="home: URI:DIR2:x:y\ndocs: URI:DIR2:d:e\nnoise\n")

        assert alias_caps(run) == {"home": "URI:DIR2:x:y", "docs": "URI:DIR2:d:e"}
        assert list_aliases(run) == ["home", "docs"]

    def test_failure_means_no_aliases(self):
        def failing(*_args, **_kwargs):
            return completed(rc=1)

        assert alias_caps(failing) == {}

    def test_web_lister_accepts_only_http(self):
        import pytest

        from redundanet.storage.inventory import web_lister

        assert callable(web_lister("http://127.0.0.1:3456/"))
        with pytest.raises(ValueError, match="http"):
            web_lister("file:///etc/passwd")

    def test_node_url(self, tmp_path: Path):
        (tmp_path / "node.url").write_text("http://127.0.0.1:3456/\n")
        assert read_node_url(tmp_path / "node.url") == "http://127.0.0.1:3456/"
        assert read_node_url(tmp_path / "missing").startswith("http://127.0.0.1:")


class TestWalk:
    def test_all_file_caps_walks_every_alias_recursively(self):
        lister = FakeLister(
            {
                "URI:DIR2:x": {"a": ("filenode", chk(1, 2, 10)), "sub": ("dirnode", "URI:DIR2:s")},
                "URI:DIR2:s": {"b": ("filenode", chk(1, 2, 20, "b"))},
                "URI:DIR2:y": {"c": ("filenode", chk(1, 2, 30, "c"))},
            }
        )
        caps = all_file_caps({"home": "URI:DIR2:x", "docs": "URI:DIR2:y"}, lister)
        assert sorted(caps) == sorted([chk(1, 2, 10), chk(1, 2, 20, "b"), chk(1, 2, 30, "c")])

    def test_paths_are_relative_to_the_root(self):
        lister = FakeLister(
            {
                "URI:DIR2:x": {
                    "a.txt": ("filenode", chk(1, 2, 1)),
                    "sub": ("dirnode", "URI:DIR2:s"),
                },
                "URI:DIR2:s": {"b.txt": ("filenode", chk(1, 2, 2, "b"))},
            }
        )
        assert sorted(walk_tree("URI:DIR2:x", lister)) == [
            ("a.txt", chk(1, 2, 1)),
            ("sub/b.txt", chk(1, 2, 2, "b")),
        ]

    def test_failed_listing_is_skipped_and_logged(self):
        logged: list[str] = []
        skipped: list[str] = []
        lister = FakeLister(
            {"URI:DIR2:x": {"gone": ("dirnode", "URI:DIR2:gone"), "f": ("filenode", chk(1, 2, 1))}}
        )
        files = walk_tree("URI:DIR2:x", lister, label="x:", log=logged.append, skipped=skipped)
        assert files == [("f", chk(1, 2, 1))]
        assert skipped == ["x:gone"]
        assert logged and "skipping" in logged[0] and "'x:gone'" in logged[0]

    def test_listing_timeout_skips_the_subtree_and_marks_it(self):
        """One directory that cannot be listed in time must not sink the walk."""
        lister = FakeLister(
            {
                "URI:DIR2:x:y": {
                    "big": ("dirnode", "URI:DIR2:big:x"),
                    "ok": ("dirnode", "URI:DIR2:ok:x"),
                    "top.bin": ("filenode", chk(2, 4, 10, "top")),
                },
                "URI:DIR2:ok:x": {"f.bin": ("filenode", chk(2, 4, 20, "f"))},
            },
            failing=("URI:DIR2:big:x",),
        )
        skipped: list[str] = []
        caps = all_file_caps({"home": "URI:DIR2:x:y"}, lister, skipped=skipped)
        assert sorted(caps) == sorted([chk(2, 4, 10, "top"), chk(2, 4, 20, "f")])
        assert skipped == ["home:big"]

    def test_parallel_listings_give_the_same_result(self):
        dirs = {"URI:DIR2:root": {f"d{i}": ("dirnode", f"URI:DIR2:d{i}") for i in range(20)}}
        for i in range(20):
            dirs[f"URI:DIR2:d{i}"] = {f"f{i}": ("filenode", chk(1, 2, i + 1, f"k{i}"))}
        sequential = sorted(walk_tree("URI:DIR2:root", FakeLister(dirs), workers=1))
        parallel = FakeLister(dirs)
        assert sorted(walk_tree("URI:DIR2:root", parallel, workers=4)) == sequential
        assert len(parallel.calls) == 21  # every directory listed exactly once


SNAPSHOT = "URI:DIR2-CHK:snap:1:2:4:100"


def snapshots() -> FakeLister:
    """tahoe backup links a new Archives/<timestamp> on every run; unchanged
    runs point at the same immutable directory."""
    return FakeLister(
        {
            "URI:DIR2:x": {
                "Archives": ("dirnode", "URI:DIR2:arch"),
                "Latest": ("dirnode", SNAPSHOT),
            },
            "URI:DIR2:arch": {
                "2026-09-05_01": ("dirnode", SNAPSHOT),
                "2026-09-05_02": ("dirnode", SNAPSHOT),
                "2026-09-05_03": ("dirnode", SNAPSHOT),
            },
            SNAPSHOT: {"f": ("filenode", chk(1, 2, 100))},
        }
    )


class TestSnapshots:
    def test_identical_snapshot_directories_are_listed_once(self):
        lister = snapshots()
        files = walk_tree("URI:DIR2:x", lister)
        assert len(files) == 1
        assert lister.calls.count(SNAPSHOT) == 1

    def test_rebalancer_view_skips_immutable_snapshots(self):
        lister = snapshots()
        assert walk_tree("URI:DIR2:x", lister, skip_immutable=True) == []
        assert SNAPSHOT not in lister.calls


class TestDirectoryCache:
    def test_immutable_directories_come_from_the_cache_the_second_time(self):
        cache = DirCache()
        first = snapshots()
        assert len(walk_tree("URI:DIR2:x", first, cache=cache)) == 1
        assert cache.size() == 1  # the snapshot; the mutable root and Archives are not cached
        second = snapshots()
        assert len(walk_tree("URI:DIR2:x", second, cache=cache)) == 1
        assert SNAPSHOT not in second.calls
        assert "URI:DIR2:x" in second.calls and "URI:DIR2:arch" in second.calls

    def test_cache_round_trip_and_reopen(self, tmp_path: Path):
        path = tmp_path / "dircache.sqlite"
        cache = DirCache(path)
        assert cache.get("URI:DIR2-CHK:a:b:1:2:3") is None
        children = [("b", "filenode", chk(1, 2, 1)), ("a", "dirnode", "URI:DIR2-CHK:c:d:1:2:3")]
        cache.put("URI:DIR2-CHK:a:b:1:2:3", children, now=1.0)
        cache.put("URI:DIR2-CHK:empty:x:1:2:3", [], now=1.0)
        cache.close()
        again = DirCache(path)
        assert again.get("URI:DIR2-CHK:a:b:1:2:3") == sorted(children)
        assert again.get("URI:DIR2-CHK:empty:x:1:2:3") == []
        assert again.size() == 2

    def test_a_cached_listing_is_walked_like_a_fresh_one(self):
        cache = DirCache()
        inner = "URI:DIR2-CHK:inner:x:1:2:3"
        cache.put(SNAPSHOT, [("inner", "dirnode", inner), ("f", "filenode", chk(1, 2, 1))])
        cache.put(inner, [("g", "filenode", chk(1, 2, 2, "g"))])
        lister = FakeLister({"URI:DIR2:x": {"Latest": ("dirnode", SNAPSHOT)}})
        files = walk_tree("URI:DIR2:x", lister, cache=cache)
        assert sorted(files) == [("Latest/f", chk(1, 2, 1)), ("Latest/inner/g", chk(1, 2, 2, "g"))]
        assert lister.calls == ["URI:DIR2:x"]


def manifest() -> dict:
    return {
        "network": {
            "tahoe": {"shares_needed": 1, "shares_happy": 2, "shares_total": 2},
            "quota": {"reserve": 0, "enforce": True},
        },
        "nodes": [
            {
                "name": "n1",
                "member": "ale",
                "roles": ["tahoe_storage", "tahoe_client"],
                "storage_contribution": "100GB",
            }
        ],
    }


class TestUsagePayload:
    def test_payload_fields(self):
        now = datetime(2026, 9, 6, 12, 0, tzinfo=UTC)
        payload = usage_payload(
            "n1", manifest(), Footprint(used_bytes=20, data_bytes=10, files=1), now=now
        )
        assert payload["member"] == "ale"
        assert payload["allocation_bytes"] == 50 * 10**9
        assert (payload["used_bytes"], payload["files"], payload["encoding"]) == (20, 1, "1-of-2")
        assert payload["enforce"] is True
        assert payload["computed_at"] == "2026-09-06T12:00:00+00:00"

    def test_enforce_override(self):
        payload = usage_payload("n1", manifest(), Footprint(0, 0, 0), enforce_override=False)
        assert payload["enforce"] is False

    def test_in_progress_defaults_to_zero_and_is_reported_separately(self):
        payload = usage_payload("n1", manifest(), Footprint(20, 10, 1))
        assert (payload["in_progress_files"], payload["in_progress_bytes"]) == (0, 0)
        payload = usage_payload(
            "n1", manifest(), Footprint(120, 60, 6), in_progress=Footprint(100, 50, 5)
        )
        # used_bytes is the whole footprint; in_progress is the unlinked part of it
        assert payload["used_bytes"] == 120 and payload["files"] == 6
        assert (payload["in_progress_files"], payload["in_progress_bytes"]) == (5, 100)

    def test_unknown_manifest_means_no_allocation(self):
        payload = usage_payload("n1", {}, Footprint(0, 0, 0))
        assert payload["allocation_bytes"] == 0
        assert payload["member"] == "n1"


class TestUsageFile:
    def test_round_trip_and_missing(self, tmp_path: Path):
        path = tmp_path / "usage.json"
        assert load_usage_file(path) is None
        write_usage_file({"used_bytes": 5}, path)
        assert load_usage_file(path) == {"used_bytes": 5}
        path.write_text("not json")
        assert load_usage_file(path) is None


class TestOverAllocation:
    def test_only_when_enforced(self):
        report = {"enforce": False, "used_bytes": 900, "allocation_bytes": 100}
        assert over_allocation(report) is False
        report["enforce"] = True
        assert over_allocation(report) is True

    def test_extra_bytes_counted(self):
        report = {"enforce": True, "used_bytes": 90, "allocation_bytes": 100}
        assert over_allocation(report, 10) is False
        assert over_allocation(report, 11) is True

    def test_garbage_never_blocks(self):
        assert over_allocation({"enforce": True, "used_bytes": "x"}) is False


class TestParseEncoding:
    def test_file_and_immutable_directory_caps(self):
        assert parse_encoding("URI:CHK:key:hash:2:4:1000") == (2, 4)
        assert parse_encoding("URI:DIR2-CHK:key:hash:1:2:500") == (1, 2)

    def test_caps_without_shares_are_none(self):
        assert parse_encoding("URI:LIT:abcd") is None
        assert parse_encoding("URI:DIR2:writekey:fingerprint") is None
        assert parse_encoding("URI:DIR2-LIT:abcd") is None
        assert parse_encoding("URI:CHK:key:hash:x:4:1000") is None
        assert parse_encoding("garbage") is None
