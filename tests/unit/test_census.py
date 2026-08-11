"""Unit tests for the share census and per-object replication computation."""

from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

from redundanet.monitor.census import census_payload, disk_used_bytes, list_storage_indexes
from redundanet.monitor.render import render_html
from redundanet.monitor.status import collect_status

NOW = datetime(2026, 8, 10, 12, 0, 0, tzinfo=UTC)


def make_shares(tmp_path: Path, indexes: dict[str, list[int]]) -> Path:
    """Build a Tahoe-style shares tree: shares/<prefix>/<si>/<sharenum>."""
    shares = tmp_path / "shares"
    for si, sharenums in indexes.items():
        si_dir = shares / si[:2] / si
        si_dir.mkdir(parents=True)
        for num in sharenums:
            (si_dir / str(num)).write_bytes(b"x" * 100)
    return shares


class TestCensus:
    def test_lists_storage_indexes(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0, 1], "bbindex2": [0]})
        assert list_storage_indexes(shares) == ["aaindex1", "bbindex2"]
        assert disk_used_bytes(shares) == 300

    def test_ignores_incoming_and_empty_dirs(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        (shares / "incoming" / "zz" / "zzindex9").mkdir(parents=True)
        (shares / "cc" / "ccempty").mkdir(parents=True)  # SI dir without share files
        assert list_storage_indexes(shares) == ["aaindex1"]

    def test_missing_dir_is_empty(self, tmp_path: Path):
        assert list_storage_indexes(tmp_path / "nope") == []
        assert census_payload("n1", tmp_path / "nope") == {
            "node": "n1",
            "object_count": 0,
            "storage_indexes": [],
            "disk_used_bytes": 0,
        }


def manifest() -> dict:
    return {
        "network": {
            "name": "redundanet",
            "tahoe": {"shares_needed": 1, "shares_happy": 2, "shares_total": 2},
        },
        "nodes": [
            {
                "name": "hub",
                "vpn_ip": "10.100.0.1",
                "roles": ["tahoe_introducer"],
                "status": "active",
            },
            {"name": "n1", "vpn_ip": "10.100.0.10", "roles": ["tahoe_storage"], "status": "active"},
            {"name": "n2", "vpn_ip": "10.100.0.11", "roles": ["tahoe_storage"], "status": "active"},
        ],
    }


def censuses(mapping: dict[str, dict | None]):
    def fetch(vpn_ip: str) -> dict | None:
        return mapping.get(vpn_ip)

    return fetch


def payload(indexes: list[str], disk: int = 1000) -> dict:
    return {
        "object_count": len(indexes),
        "storage_indexes": indexes,
        "disk_used_bytes": disk,
    }


def collect(fetch, storage_connected: int = 2):
    return collect_status(
        manifest(),
        "hub",
        lambda _ip: 5.0,
        storage_connected=storage_connected,
        furl_present=True,
        manifest_synced_at=NOW,
        now=NOW,
        fetch_census=fetch,
    )


class TestReplication:
    def test_fully_replicated(self):
        status = collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1", "si2"]),
                    "10.100.0.11": payload(["si1", "si2"]),
                }
            )
        )
        replication = status.replication
        assert replication is not None
        assert replication.objects_total == 2
        assert replication.target_copies == 2
        assert replication.fully_replicated == 2
        assert replication.under_replicated == 0
        assert replication.complete
        assert status.overall == "ok"

    def test_under_replicated_object_degrades(self):
        status = collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1", "si2"]),
                    "10.100.0.11": payload(["si1"]),  # si2 only on n1
                }
            )
        )
        assert status.replication.under_replicated == 1
        assert status.overall == "degraded"
        assert any("re-upload or repair" in note for note in status.notes)

    def test_missing_census_is_partial_not_alarming(self):
        """A node that doesn't answer must not make its objects look at risk."""
        status = collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1", "si2"]),
                    "10.100.0.11": None,
                }
            )
        )
        replication = status.replication
        assert replication is not None
        assert not replication.complete
        assert status.overall == "ok"  # no false alarm
        assert any("census unavailable from n2" in note for note in status.notes)

    def test_all_censuses_missing(self):
        status = collect(censuses({}))
        assert status.replication is None
        assert status.overall == "ok"

    def test_no_fetcher_means_no_replication_section(self):
        status = collect_status(
            manifest(),
            "hub",
            lambda _ip: 5.0,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
        )
        assert status.replication is None

    def test_json_has_aggregates_but_never_raw_indexes(self):
        status = collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1"]),
                    "10.100.0.11": payload(["si1"]),
                }
            )
        )
        data = status.to_dict()
        assert data["replication"]["fully_replicated"] == 1
        assert data["replication"]["per_server"]["n1"]["objects"] == 1
        assert "si1" not in str(data)

    def test_page_shows_replication_tile_and_stored_column(self):
        status = collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1", "si2"], disk=2048),
                    "10.100.0.11": payload(["si1", "si2"], disk=2048),
                }
            )
        )
        html = render_html(status)
        assert "objects fully replicated" in html
        assert "2/2" in html
        assert "2 obj · 2.0 KB" in html

    def test_partial_census_shows_unknown_not_alarm(self):
        status = collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1"]),
                    "10.100.0.11": None,
                }
            )
        )
        assert "?/1" in render_html(status)
