"""Unit tests for the alias walker / grid footprint (storage/inventory.py) and
the usage reports (monitor/usage.py)."""

from __future__ import annotations

import json
import subprocess
from datetime import UTC, datetime
from pathlib import Path

from redundanet.monitor.usage import (
    load_usage_file,
    over_allocation,
    usage_payload,
    write_usage_file,
)
from redundanet.storage.inventory import (
    Footprint,
    all_file_caps,
    grid_footprint,
    parse_chk,
    parse_chk_params,
    walk_files,
)


def chk(k: int, n: int, size: int, key: str = "aaaa") -> str:
    return f"URI:CHK:{key}:hash:{k}:{n}:{size}"


def completed(rc: int = 0, stdout: str = "", stderr: str = ""):
    return subprocess.CompletedProcess(args=["tahoe"], returncode=rc, stdout=stdout, stderr=stderr)


def dirnode_json(children: dict) -> str:
    payload = {
        name: [kind, {"ro_uri": cap} if cap else {}] for name, (kind, cap) in children.items()
    }
    return json.dumps(["dirnode", {"children": payload}])


class FakeRun:
    def __init__(self, responses: dict):
        self.responses = responses
        self.calls: list[list[str]] = []

    def __call__(self, args, timeout=3600):
        self.calls.append(list(args))
        return self.responses.get((args[0], args[-1])) or self.responses.get(args[0]) or completed()


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


class TestWalk:
    def test_all_file_caps_walks_every_alias_recursively(self):
        run = FakeRun(
            {
                "list-aliases": completed(stdout="home: URI:DIR2:x\ndocs: URI:DIR2:y\n"),
                ("ls", "home:"): completed(
                    stdout=dirnode_json(
                        {"a": ("filenode", chk(1, 2, 10)), "sub": ("dirnode", "URI:DIR2:s")}
                    )
                ),
                ("ls", "home:sub"): completed(
                    stdout=dirnode_json({"b": ("filenode", chk(1, 2, 20, "b"))})
                ),
                ("ls", "docs:"): completed(
                    stdout=dirnode_json({"c": ("filenode", chk(1, 2, 30, "c"))})
                ),
            }
        )
        assert sorted(all_file_caps(run)) == sorted(
            [chk(1, 2, 10), chk(1, 2, 20, "b"), chk(1, 2, 30, "c")]
        )

    def test_failed_listing_is_skipped_and_logged(self):
        logged: list[str] = []
        run = FakeRun({("ls", "x:"): completed(rc=1, stderr="boom")})
        assert walk_files("x:", run, log=logged.append) == []
        assert logged and "skipping" in logged[0]


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


class TestSnapshots:
    """tahoe backup links a new Archives/<timestamp> on every run; unchanged runs
    point at the same immutable directory."""

    def snapshots(self) -> FakeRun:
        snapshot = dirnode_json({"f": ("filenode", chk(1, 2, 100))})
        return FakeRun(
            {
                "list-aliases": completed(stdout="backups: URI:DIR2:x\n"),
                ("ls", "backups:"): completed(
                    stdout=dirnode_json(
                        {
                            "Archives": ("dirnode", "URI:DIR2:arch"),
                            "Latest": ("dirnode", "URI:DIR2-CHK:snap:1"),
                        }
                    )
                ),
                ("ls", "backups:Archives"): completed(
                    stdout=dirnode_json(
                        {
                            "2026-09-05_01": ("dirnode", "URI:DIR2-CHK:snap:1"),
                            "2026-09-05_02": ("dirnode", "URI:DIR2-CHK:snap:1"),
                            "2026-09-05_03": ("dirnode", "URI:DIR2-CHK:snap:1"),
                        }
                    )
                ),
                ("ls", "backups:Latest"): completed(stdout=snapshot),
                ("ls", "backups:Archives/2026-09-05_01"): completed(stdout=snapshot),
                ("ls", "backups:Archives/2026-09-05_02"): completed(stdout=snapshot),
                ("ls", "backups:Archives/2026-09-05_03"): completed(stdout=snapshot),
            }
        )

    def test_identical_snapshot_directories_are_listed_once(self):
        run = self.snapshots()
        files = walk_files("backups:", run)
        # One file, reached through the first link to that directory only.
        assert len(files) == 1
        listed = [c[-1] for c in run.calls if c[0] == "ls"]
        assert listed.count("backups:Latest") + sum(1 for s in listed if "Archives/" in s) == 1

    def test_rebalancer_view_skips_immutable_snapshots(self):
        run = self.snapshots()
        assert walk_files("backups:", run, skip_immutable=True) == []
        assert not any("Archives/" in c[-1] or c[-1] == "backups:Latest" for c in run.calls)

    def test_footprint_counts_each_capability_once(self):
        footprint = grid_footprint([chk(1, 2, 100)] * 50 + [chk(1, 2, 100, "other")])
        assert footprint == Footprint(used_bytes=400, data_bytes=200, files=2)
