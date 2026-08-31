"""Unit tests for the share census and per-object replication computation."""

from __future__ import annotations

from datetime import UTC, datetime
from pathlib import Path

from redundanet.monitor.census import census_payload, disk_used_bytes, list_storage_indexes
from redundanet.monitor.render import render_html
from redundanet.monitor.status import collect_status, load_census, save_census

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


def collect(fetch, storage_connected: int = 2, cache_dir: Path | None = None):
    return collect_status(
        manifest(),
        "hub",
        lambda _ip: 5.0,
        storage_connected=storage_connected,
        furl_present=True,
        manifest_synced_at=NOW,
        now=NOW,
        fetch_census=fetch,
        census_cache_dir=cache_dir,
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

    def test_missing_census_without_history_assumes_empty(self):
        """A node that never reported is assumed empty (true for a new node)
        and must not raise a false alarm."""
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
        assert replication.per_server["n2"].source == "assumed-empty"
        assert status.overall == "ok"  # readable (needed=1), so no alarm
        assert any("never reported a census; assuming empty" in note for note in status.notes)

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

    def test_partial_census_shows_counts_with_marker(self):
        """A missing node no longer blanks the tile to '?': counts are shown
        with an asterisk and an availability line."""
        status = collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1"]),
                    "10.100.0.11": None,
                }
            )
        )
        html = render_html(status)
        assert "0/1*" in html  # n2 assumed empty -> si1 on 1 of 2 target servers
        assert "fully reachable" in html


class TestCensusCache:
    def test_save_and_load_round_trip_with_age(self, tmp_path: Path):
        save_census(tmp_path, "n2", payload(["si1"]), NOW)
        loaded = load_census(tmp_path, "n2", NOW)
        assert loaded is not None
        record, age = loaded
        assert record["storage_indexes"] == ["si1"]
        assert age == 0.0
        assert load_census(tmp_path, "ghost", NOW) is None

    def test_offline_node_uses_cached_inventory(self, tmp_path: Path):
        both = censuses(
            {
                "10.100.0.10": payload(["si1", "si2"]),
                "10.100.0.11": payload(["si1", "si2"]),
            }
        )
        collect(both, cache_dir=tmp_path)  # first cycle: caches both

        status = collect(
            censuses({"10.100.0.10": payload(["si1", "si2"]), "10.100.0.11": None}),
            cache_dir=tmp_path,
        )
        replication = status.replication
        assert replication is not None
        # Placement still counts the offline node's (cached) copies...
        assert replication.fully_replicated == 2
        assert not replication.complete
        assert replication.per_server["n2"].source == "cached"
        # ...while availability reflects only live servers.
        assert replication.available_full == 0
        assert replication.unreadable_now == 0  # needed=1, n1 still serves both
        assert status.overall == "ok"
        assert any("using its census from" in note for note in status.notes)
        assert "2/2*" in render_html(status)

    def test_object_only_on_offline_node_is_unreadable_and_degrades(self, tmp_path: Path):
        collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1"]),
                    "10.100.0.11": payload(["si1", "si2"]),  # si2 lives only here
                }
            ),
            cache_dir=tmp_path,
        )
        status = collect(
            censuses({"10.100.0.10": payload(["si1"]), "10.100.0.11": None}),
            cache_dir=tmp_path,
        )
        replication = status.replication
        assert replication is not None
        assert replication.unreadable_now == 1  # si2 has 0 live copies < needed=1
        assert status.overall == "degraded"
        assert any("unreadable until a server returns" in note for note in status.notes)
