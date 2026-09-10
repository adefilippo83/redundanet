"""Unit tests for the hub status monitor (model, history, renderer)."""

from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from pathlib import Path

from redundanet.monitor.render import render_html
from redundanet.monitor.status import (
    MAX_ROLLUP_LINES,
    append_sample,
    collect_status,
    rollup_hours,
    uptime_stats,
    uptime_windows,
)

NOW = datetime(2026, 8, 10, 12, 0, 0, tzinfo=UTC)


def manifest(nodes: list[dict] | None = None, happy: int = 2) -> dict:
    return {
        "network": {
            "name": "redundanet",
            "tahoe": {"shares_needed": 1, "shares_happy": happy, "shares_total": 2},
        },
        "nodes": nodes
        or [
            {
                "name": "hub",
                "vpn_ip": "10.100.0.1",
                "roles": ["tinc_vpn", "tahoe_introducer"],
                "status": "active",
            },
            {
                "name": "n1",
                "vpn_ip": "10.100.0.10",
                "roles": ["tinc_vpn", "tahoe_storage"],
                "status": "pending",
            },
            {
                "name": "n2",
                "vpn_ip": "10.100.0.11",
                "roles": ["tinc_vpn", "tahoe_storage"],
                "status": "pending",
            },
        ],
    }


def all_up(_ip: str) -> float:
    return 12.5


def all_down(_ip: str) -> None:
    return None


class TestVerdict:
    def test_everything_healthy_is_ok(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.overall == "ok"
        assert status.notes == []
        assert status.grid.uploads_possible is True
        assert status.grid.tolerable_failures == 1  # 1-of-2 mirror

    def test_unreachable_node_degrades(self):
        def one_down(ip: str) -> float | None:
            return None if ip == "10.100.0.11" else 10.0

        status = collect_status(
            manifest(),
            "hub",
            one_down,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.overall == "degraded"
        assert any("n2" in note for note in status.notes)

    def test_inactive_nodes_do_not_degrade(self):
        nodes = manifest()["nodes"]
        nodes[2]["status"] = "inactive"
        status = collect_status(
            manifest(nodes),
            "hub",
            lambda ip: None if ip == "10.100.0.11" else 10.0,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.overall == "ok"

    def test_too_few_servers_for_uploads_degrades(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=1,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.overall == "degraded"
        assert status.grid.uploads_possible is False

    def test_introducer_unqueryable_degrades(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=None,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.overall == "degraded"
        assert status.grid.uploads_possible is None

    def test_stale_manifest_degrades(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW - timedelta(hours=3),
            now=NOW,
        )
        assert status.overall == "degraded"

    def test_missing_furl_is_down(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=False,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.overall == "down"

    def test_fewer_servers_than_needed_is_down(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=0,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.overall == "down"

    def test_self_never_pinged(self):
        pinged: list[str] = []

        def track(ip: str) -> float:
            pinged.append(ip)
            return 1.0

        collect_status(
            manifest(),
            "hub",
            track,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert "10.100.0.1" not in pinged
        assert len(pinged) == 2


class TestHistory:
    def test_uptime_from_samples(self, tmp_path: Path):
        history = tmp_path / "history.jsonl"
        for minute in range(10):
            status = collect_status(
                manifest(),
                "hub",
                (all_down if minute < 2 else all_up),  # n1/n2 down for 2 of 10 samples
                storage_connected=2,
                furl_present=True,
                manifest_synced_at=NOW,
                now=NOW + timedelta(minutes=minute),
            )
            append_sample(history, status)

        stats = uptime_stats(history, timedelta(hours=24), now=NOW + timedelta(minutes=10))
        assert stats["hub"] == 100.0  # self is always up
        assert stats["n1"] == 80.0

    def test_old_samples_excluded(self, tmp_path: Path):
        history = tmp_path / "history.jsonl"
        old = collect_status(
            manifest(),
            "hub",
            all_down,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW - timedelta(days=2),
        )
        append_sample(history, old)
        recent = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        append_sample(history, recent)
        stats = uptime_stats(history, timedelta(hours=24), now=NOW)
        assert stats["n1"] == 100.0

    def test_corrupt_lines_ignored(self, tmp_path: Path):
        history = tmp_path / "history.jsonl"
        history.write_text("not json\n")
        assert uptime_stats(history, timedelta(hours=24), now=NOW) == {}


class TestRender:
    def test_page_contains_key_facts(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        status.nodes[1].uptime_24h = 99.5
        html = render_html(status)
        assert "All systems operational" in html
        assert "3/3" in html  # nodes online
        assert "2/2" in html  # storage servers
        assert "1-of-2" in html
        assert "n1" in html and "n2" in html
        assert "99.5%" in html
        assert "status.json" in html

    def test_down_state_and_note_escaping(self):
        bad = manifest(
            [
                {
                    "name": "<script>x</script>",
                    "vpn_ip": "10.100.0.9",
                    "roles": [],
                    "status": "active",
                }
            ]
        )
        status = collect_status(
            bad,
            "hub",
            all_down,
            storage_connected=0,
            furl_present=False,
            manifest_synced_at=None,
            now=NOW,
        )
        html = render_html(status)
        assert "Down" in html
        assert "<script>x</script>" not in html  # escaped
        assert "&lt;script&gt;" in html

    def test_json_roundtrip(self):
        import json

        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        data = json.loads(json.dumps(status.to_dict()))
        assert data["overall"] == "ok"
        assert data["grid"]["tolerable_failures"] == 1
        assert len(data["nodes"]) == 3


class TestQuotas:
    """Contribution, allocation and usage per member on the status page."""

    def manifest_with_members(self) -> dict:
        return {
            "network": {
                "name": "redundanet",
                "tahoe": {"shares_needed": 1, "shares_happy": 2, "shares_total": 2},
                "quota": {"reserve": 0, "enforce": True},
            },
            "nodes": [
                {
                    "name": "hub",
                    "vpn_ip": "10.100.0.1",
                    "roles": ["tinc_vpn", "tahoe_introducer"],
                    "status": "active",
                },
                {
                    "name": "n1",
                    "vpn_ip": "10.100.0.10",
                    "roles": ["tinc_vpn", "tahoe_storage", "tahoe_client"],
                    "status": "active",
                    "member": "ale",
                    "storage_contribution": "100GB",
                },
                {
                    "name": "n2",
                    "vpn_ip": "10.100.0.11",
                    "roles": ["tinc_vpn", "tahoe_storage"],
                    "status": "active",
                    "member": "bob",
                    "storage_contribution": "100GB",
                },
            ],
        }

    def collect(self, fetch_usage=None, cache: Path | None = None):
        return collect_status(
            self.manifest_with_members(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
            fetch_usage=fetch_usage,
            usage_cache_dir=cache,
        )

    def test_allocations_without_reports(self):
        status = self.collect()
        quotas = {q.member: q for q in status.quotas}
        assert quotas["ale"].allocation_bytes == 50 * 10**9
        assert quotas["ale"].used_bytes is None
        assert status.overall == "ok"

    def test_usage_report_and_over_allocation_note(self):
        def usage(ip: str):
            return {"used_bytes": 60 * 10**9, "files": 4} if ip == "10.100.0.10" else None

        status = self.collect(fetch_usage=usage)
        ale = next(q for q in status.quotas if q.member == "ale")
        assert ale.used_bytes == 60 * 10**9
        assert ale.over is True
        assert ale.usage_source == "live"
        assert any("member ale is over allocation" in n for n in status.notes)
        assert status.overall == "ok"  # visibility, not a network fault
        data = status.to_dict()
        assert data["quotas"][0]["member"] == "ale"
        assert data["quotas"][0]["percent"] == 120.0
        assert data["quotas"][0]["over"] is True

    def test_backup_in_progress_is_visible(self):
        """A first backup links nothing until it completes; the meter reports
        what it uploaded so far and the page says so."""

        def usage(ip: str):
            if ip != "10.100.0.10":
                return None
            return {"used_bytes": 3 * 10**9, "files": 64000, "in_progress_files": 64000}

        status = self.collect(fetch_usage=usage)
        ale = next(q for q in status.quotas if q.member == "ale")
        assert ale.in_progress_files == 64000
        assert status.to_dict()["quotas"][0]["in_progress_files"] == 64000
        assert "64,000 files uploading" in render_html(status)

    def test_cached_report_used_when_node_silent(self, tmp_path: Path):
        cache = tmp_path / "usage"
        self.collect(
            fetch_usage=lambda ip: {"used_bytes": 10, "files": 1} if ip == "10.100.0.10" else None,
            cache=cache,
        )
        status = self.collect(fetch_usage=lambda _ip: None, cache=cache)
        ale = next(q for q in status.quotas if q.member == "ale")
        assert ale.used_bytes == 10
        assert ale.usage_source == "cached"

    def test_members_table_rendered(self):
        status = self.collect(
            fetch_usage=lambda ip: (
                {"used_bytes": 25 * 10**9, "files": 2} if ip == "10.100.0.10" else None
            )
        )
        html = render_html(status)
        assert "<h1>Members</h1>" in html
        assert "ale" in html
        assert "25.0 GB" in html
        assert "50.0%" in html

    def test_no_members_table_without_contributions(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert "<h1>Members</h1>" not in render_html(status)


class TestRollups:
    """7-day / 30-day uptime from hourly rollups of the minute samples."""

    @staticmethod
    def write_samples(path: Path, hours: dict[str, list[tuple[bool, bool | None]]]) -> None:
        """hours: hour key -> one (n1 up, n2 up or None when n2 absent) per minute."""
        with path.open("w") as f:
            for hour, samples in hours.items():
                for minute, (n1, n2) in enumerate(samples):
                    up: dict[str, bool] = {"n1": n1}
                    if n2 is not None:
                        up["n2"] = n2
                    f.write(
                        json.dumps(
                            {"ts": f"{hour}:{minute:02d}:00+00:00", "overall": "ok", "up": up}
                        )
                        + "\n"
                    )

    def test_completed_hours_are_folded_once(self, tmp_path: Path):
        history, rollup = tmp_path / "h.jsonl", tmp_path / "r.jsonl"
        self.write_samples(
            history,
            {
                "2026-09-06T10": [(True, True)] * 57 + [(False, True)] * 3,  # n1 57/60, n2 60/60
                "2026-09-06T11": [(True, None)] * 30,  # the current hour: still open
            },
        )
        now = datetime(2026, 9, 6, 11, 30, tzinfo=UTC)
        assert rollup_hours(history, rollup, now=now) == 1
        rows = [json.loads(line) for line in rollup.read_text().splitlines()]
        assert rows == [
            {"hour": "2026-09-06T10", "samples": 60, "up": {"n1": [57, 60], "n2": [60, 60]}}
        ]
        assert rollup_hours(history, rollup, now=now) == 0  # idempotent
        assert len(rollup.read_text().splitlines()) == 1

    def test_windows_from_rollups(self, tmp_path: Path):
        rollup = tmp_path / "r.jsonl"
        now = datetime(2026, 9, 30, 12, 0, tzinfo=UTC)

        def hour(days_ago: int, n1_up: int, seen: int = 60) -> str:
            key = (now - timedelta(days=days_ago)).strftime("%Y-%m-%dT%H")
            return json.dumps(
                {"hour": key, "samples": seen, "up": {"n1": [n1_up, seen], "n2": [seen, seen]}}
            )

        rollup.write_text("\n".join([hour(1, 60), hour(3, 30), hour(20, 0), hour(40, 0)]) + "\n")
        windows = uptime_windows(
            rollup, {"7d": timedelta(days=7), "30d": timedelta(days=30)}, now=now
        )
        assert windows["7d"] == {"n1": 75.0, "n2": 100.0}  # (60+30)/120
        assert windows["30d"] == {"n1": 50.0, "n2": 100.0}  # (60+30+0)/180; the 40-day hour is out
        assert "n3" not in windows["7d"]
        assert uptime_windows(tmp_path / "missing.jsonl", {"7d": timedelta(days=7)}, now=now) == {
            "7d": {}
        }

    def test_rollup_file_is_trimmed(self, tmp_path: Path):
        history, rollup = tmp_path / "h.jsonl", tmp_path / "r.jsonl"
        old = "\n".join(
            json.dumps({"hour": f"2020-01-01T{i % 24:02d}", "samples": 1, "up": {}})
            for i in range(2200)
        )
        rollup.write_text(old + "\n")
        self.write_samples(history, {"2026-09-06T10": [(True, True)] * 5})
        rollup_hours(history, rollup, now=datetime(2026, 9, 6, 11, 0, tzinfo=UTC))
        lines = rollup.read_text().splitlines()
        assert len(lines) == MAX_ROLLUP_LINES
        assert json.loads(lines[-1])["hour"] == "2026-09-06T10"

    def test_page_and_json_carry_the_windows(self):
        status = collect_status(
            manifest(),
            "hub",
            all_up,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        for node in status.nodes:
            node.uptime_24h, node.uptime_7d, node.uptime_30d = 100.0, 99.5, 98.2
        html = render_html(status)
        assert "<th>7 days</th>" in html
        assert "99.5%" in html
        assert "98.2%" in html
        assert status.to_dict()["nodes"][0]["uptime_30d"] == 98.2
