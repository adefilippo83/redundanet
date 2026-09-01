"""Unit tests for the automatic re-encoder (docker/entrypoints/rebalance.py)."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import rebalance  # noqa: E402


def completed(returncode: int = 0, stdout: str = "", stderr: str = ""):
    return subprocess.CompletedProcess(
        args=["tahoe"], returncode=returncode, stdout=stdout, stderr=stderr
    )


def chk(k: int, n: int, key: str = "aaaa") -> str:
    return f"URI:CHK:{key}:hash:{k}:{n}:12345"


def dirnode_json(children: dict) -> str:
    """children: name -> ('filenode', cap) | ('dirnode', ...)"""
    payload = {}
    for name, (child_type, cap) in children.items():
        payload[name] = [child_type, {"ro_uri": cap} if cap else {}]
    return json.dumps(["dirnode", {"children": payload}])


class FakeRun:
    """Scripted tahoe CLI keyed on (subcommand, last arg)."""

    def __init__(self, responses: dict):
        self.responses = responses
        self.calls: list[list[str]] = []

    def __call__(self, args, timeout=3600):
        self.calls.append(list(args))
        key = (args[0], args[-1])
        if key in self.responses:
            return self.responses[key]
        if args[0] in self.responses:
            return self.responses[args[0]]
        return completed()


class TestParseChkParams:
    def test_extracts_k_and_n(self):
        assert rebalance.parse_chk_params(chk(1, 2)) == (1, 2)
        assert rebalance.parse_chk_params(chk(3, 10)) == (3, 10)

    def test_non_chk_caps_are_none(self):
        assert rebalance.parse_chk_params("URI:LIT:krugkidfnzsc4") is None
        assert rebalance.parse_chk_params("URI:DIR2:abc:def") is None
        assert rebalance.parse_chk_params("URI:SSK:abc:def") is None
        assert rebalance.parse_chk_params("garbage") is None
        assert rebalance.parse_chk_params("URI:CHK:a:b:notint:2:3") is None


class TestParseConfig:
    def test_default_on(self):
        config = rebalance.parse_config({})
        assert config.enabled is True
        assert config.interval == 86400
        assert config.budget == 14400

    def test_disable_and_overrides(self):
        config = rebalance.parse_config(
            {
                "REDUNDANET_REBALANCE_ENABLED": "false",
                "REDUNDANET_SHARES_NEEDED": "2",
                "REDUNDANET_SHARES_TOTAL": "4",
                "REDUNDANET_REBALANCE_PAUSE": "0",
            }
        )
        assert config.enabled is False
        assert (config.needed, config.total) == (2, 4)
        assert config.pause == 0

    def test_bad_int_falls_back(self):
        assert rebalance.parse_config({"REDUNDANET_REBALANCE_INTERVAL": "x"}).interval == 86400


class TestWalkFiles:
    def test_recurses_directories(self):
        run = FakeRun(
            {
                ("ls", "home:"): completed(
                    stdout=dirnode_json(
                        {"a.txt": ("filenode", chk(1, 2)), "sub": ("dirnode", "URI:DIR2:x:y")}
                    )
                ),
                ("ls", "home:sub"): completed(
                    stdout=dirnode_json({"b.txt": ("filenode", chk(2, 4, "bb"))})
                ),
            }
        )
        files = rebalance.walk_files("home:", run=run)
        assert ("a.txt", chk(1, 2)) in files
        assert ("sub/b.txt", chk(2, 4, "bb")) in files

    def test_listing_failure_is_skipped_not_fatal(self):
        run = FakeRun({("ls", "home:"): completed(returncode=1, stderr="boom")})
        assert rebalance.walk_files("home:", run=run) == []


def config(needed=2, total=4, budget=14400, pause=0) -> rebalance.RebalanceConfig:
    return rebalance.RebalanceConfig(
        enabled=True, interval=1, pause=pause, budget=budget, needed=needed, total=total
    )


class TestRunCycle:
    def responses(self, listing: str) -> dict:
        return {
            "list-aliases": completed(stdout="home: URI:DIR2:x:y\n"),
            ("ls", "home:"): completed(stdout=listing),
            "get": completed(),
            "put": completed(stdout=chk(2, 4)),
        }

    def test_mismatched_file_is_reencoded(self):
        run = FakeRun(self.responses(dirnode_json({"old.bin": ("filenode", chk(1, 2))})))
        stats = rebalance.run_cycle(config(), run=run, sleep=lambda _s: None)
        assert stats["reencoded"] == 1 and stats["failed"] == 0
        # get by cap, then put relinking the same path
        assert ["get", chk(1, 2), str(rebalance.TMP_FILE)] in run.calls
        assert ["put", str(rebalance.TMP_FILE), "home:old.bin"] in run.calls

    def test_matching_and_lit_files_are_skipped(self):
        run = FakeRun(
            self.responses(
                dirnode_json(
                    {
                        "ok.bin": ("filenode", chk(2, 4)),
                        "tiny.txt": ("filenode", "URI:LIT:abcd"),
                    }
                )
            )
        )
        stats = rebalance.run_cycle(config(), run=run, sleep=lambda _s: None)
        assert stats["scanned"] == 2
        assert stats["mismatched"] == 0
        assert not any(c[0] == "get" for c in run.calls)

    def test_budget_stops_cycle_early(self):
        listing = dirnode_json(
            {"a": ("filenode", chk(1, 2, "aa")), "b": ("filenode", chk(1, 2, "bb"))}
        )
        run = FakeRun(self.responses(listing))
        ticks = iter([0, 0, 99999])  # start, first check ok... second over budget
        stats = rebalance.run_cycle(
            config(budget=100), run=run, sleep=lambda _s: None, clock=lambda: next(ticks)
        )
        assert stats["reencoded"] == 1
        assert stats["budget_stop"] == 1

    def test_failed_reencode_counted_and_loop_continues(self):
        responses = self.responses(
            dirnode_json({"a": ("filenode", chk(1, 2, "aa")), "b": ("filenode", chk(1, 2, "bb"))})
        )
        responses["get"] = completed(returncode=1, stderr="no shares")
        run = FakeRun(responses)
        stats = rebalance.run_cycle(config(), run=run, sleep=lambda _s: None)
        assert stats["failed"] == 2
        assert stats["reencoded"] == 0
