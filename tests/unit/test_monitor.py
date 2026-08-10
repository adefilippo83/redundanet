"""Unit tests for the hub status monitor (model, history, renderer)."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path

from redundanet.monitor.render import render_html
from redundanet.monitor.status import append_sample, collect_status, uptime_stats

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
