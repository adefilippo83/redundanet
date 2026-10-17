"""Unit tests for the share census and per-object replication computation."""

from __future__ import annotations

import json
import os
import shutil
from datetime import UTC, datetime
from pathlib import Path

from redundanet.monitor.census import (
    HISTORY_MAX_CHANGE,
    Inventory,
    census_payload,
    delta_payload,
    disk_used_bytes,
    inventory_payload,
    list_storage_indexes,
    scan_shares,
)
from redundanet.monitor.census_store import CensusStore
from redundanet.monitor.render import render_html
from redundanet.monitor.status import ThrottledFetcher, collect_status, load_census, save_census

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


class TestScanShares:
    def test_one_pass_gives_indexes_and_bytes(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0, 1], "bbindex2": [0]})
        assert scan_shares(shares) == (["aaindex1", "bbindex2"], 300)

    def test_incoming_counts_bytes_but_never_indexes(self, tmp_path: Path):
        """Tahoe stages uploads in flight under incoming/: they occupy the disk
        but are not shares anyone can read yet."""
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        staging = shares / "incoming" / "zz" / "zzindex9"
        staging.mkdir(parents=True)
        (staging / "0").write_bytes(b"y" * 50)
        assert scan_shares(shares) == (["aaindex1"], 150)

    def test_symlinks_and_stray_files_are_ignored(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        (shares / "aa" / "aaindex1" / "link").symlink_to(shares / "aa" / "aaindex1" / "0")
        (shares / "stray.txt").write_bytes(b"z" * 10)
        assert scan_shares(shares) == (["aaindex1"], 100)

    def test_payload_carries_computed_at(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        payload = census_payload("n1", shares, now=NOW)
        assert payload["computed_at"] == "2026-08-10T12:00:00+00:00"
        assert payload["object_count"] == 1 and payload["disk_used_bytes"] == 100


class TestThrottledFetcher:
    def test_asks_again_only_after_ttl(self):
        calls: list[str] = []
        clock = [0.0]

        def fetch(addr: str) -> dict:
            calls.append(addr)
            return {"storage_indexes": [addr, str(len(calls))]}

        fetcher = ThrottledFetcher(fetch, ttl=300, clock=lambda: clock[0])
        first = fetcher("10.0.0.1")
        clock[0] = 120
        assert fetcher("10.0.0.1") is first  # served from memory
        clock[0] = 301
        assert fetcher("10.0.0.1") is not first
        assert calls == ["10.0.0.1", "10.0.0.1"]

    def test_addresses_are_independent(self):
        fetcher = ThrottledFetcher(lambda a: {"node": a}, ttl=300, clock=lambda: 0.0)
        assert fetcher("a")["node"] == "a" and fetcher("b")["node"] == "b"

    def test_failure_is_not_cached(self):
        answers = iter([None, {"ok": 1}])
        calls = 0

        def fetch(_addr: str):
            nonlocal calls
            calls += 1
            return next(answers)

        fetcher = ThrottledFetcher(fetch, ttl=300, clock=lambda: 0.0)
        assert fetcher("a") is None
        assert fetcher("a") == {"ok": 1}  # asked again at once, outage seen immediately
        assert calls == 2

    def test_stale_answer_kept_over_a_failed_refresh(self):
        """Between refreshes the last answer stands; a failed refresh returns
        None so the collector applies its own unreachable handling."""
        answers = iter([{"v": 1}, None])
        clock = [0.0]
        fetcher = ThrottledFetcher(lambda _a: next(answers), ttl=10, clock=lambda: clock[0])
        assert fetcher("a") == {"v": 1}
        clock[0] = 11
        assert fetcher("a") is None


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
        payload = census_payload("n1", tmp_path / "nope")
        assert {
            k: payload[k] for k in ("node", "object_count", "storage_indexes", "disk_used_bytes")
        } == {
            "node": "n1",
            "object_count": 0,
            "storage_indexes": [],
            "disk_used_bytes": 0,
        }
        # Disk capacity (to verify a node's claimed contribution) comes from the
        # filesystem holding the shares; a missing shares dir falls back to its parent.
        assert payload["disk_total_bytes"] is not None and payload["disk_total_bytes"] > 0
        assert payload["disk_free_bytes"] is not None


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


def collect(fetch, storage_connected: int = 2, store: CensusStore | None = None):
    return collect_status(
        manifest(),
        "hub",
        lambda _ip: 5.0,
        storage_connected=storage_connected,
        furl_present=True,
        manifest_synced_at=NOW,
        now=NOW,
        fetch_census=fetch,
        census_store=store,
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
        store = CensusStore(tmp_path / "census.sqlite")
        both = censuses(
            {
                "10.100.0.10": payload(["si1", "si2"]),
                "10.100.0.11": payload(["si1", "si2"]),
            }
        )
        collect(both, store=store)  # first cycle: the store holds both

        status = collect(
            censuses({"10.100.0.10": payload(["si1", "si2"]), "10.100.0.11": None}),
            store=store,
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
        store = CensusStore(tmp_path / "census.sqlite")
        collect(
            censuses(
                {
                    "10.100.0.10": payload(["si1"]),
                    "10.100.0.11": payload(["si1", "si2"]),  # si2 lives only here
                }
            ),
            store=store,
        )
        status = collect(
            censuses({"10.100.0.10": payload(["si1"]), "10.100.0.11": None}),
            store=store,
        )
        replication = status.replication
        assert replication is not None
        assert replication.unreadable_now == 1  # si2 has 0 live copies < needed=1
        assert status.overall == "degraded"
        assert any("unreadable until a server returns" in note for note in status.notes)


# --- the incremental inventory -----------------------------------------------

T0 = 1_700_000_000 * 10**9  # an old, fixed clock for the trees below


def settle(shares: Path, when_ns: int = T0) -> None:
    """Give every prefix directory an old mtime, so the tree looks like it
    was last modified long before the inventory listed it."""
    for prefix in shares.iterdir():
        if prefix.is_dir():
            os.utime(prefix, ns=(when_ns, when_ns))


def add_share(shares: Path, si: str, size: int = 100, num: int = 0) -> None:
    si_dir = shares / si[:2] / si
    si_dir.mkdir(parents=True, exist_ok=True)
    (si_dir / str(num)).write_bytes(b"x" * size)


class TestInventory:
    def test_first_refresh_is_a_full_walk(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0, 1], "bbindex2": [0]})
        settle(shares)
        inventory = Inventory()
        walk = inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        assert (walk.mode, walk.prefixes, walk.listed, walk.added, walk.removed) == (
            "full",
            2,
            2,
            2,
            0,
        )
        assert inventory.storage_indexes() == ["aaindex1", "bbindex2"]
        assert inventory.disk_used_bytes == 300
        assert inventory.generation == 1
        assert inventory.full_walk_at_ns == T0 + 60 * 10**9

    def test_unchanged_prefixes_are_not_listed_again(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0], "bbindex2": [0]})
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        walk = inventory.refresh(shares, now_ns=T0 + 360 * 10**9)
        assert (walk.mode, walk.listed, walk.added, walk.removed) == ("incremental", 0, 0, 0)
        assert inventory.generation == 1  # nothing changed, no new generation

    def test_an_upload_changes_only_its_prefix(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0], "bbindex2": [0]})
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        add_share(shares, "bbindex3")  # the prefix directory's mtime moves to now
        walk = inventory.refresh(shares, now_ns=T0 + 360 * 10**9)
        assert (walk.listed, walk.added, walk.removed) == (1, 1, 0)
        assert inventory.storage_indexes() == ["aaindex1", "bbindex2", "bbindex3"]
        assert inventory.changes_since(1) == ({"bbindex3"}, set())

    def test_garbage_collection_is_seen_as_a_removal(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0], "aaindex2": [0]})
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        shutil.rmtree(shares / "aa" / "aaindex2")
        walk = inventory.refresh(shares, now_ns=T0 + 360 * 10**9)
        assert (walk.added, walk.removed) == (0, 1)
        assert inventory.changes_since(1) == (set(), {"aaindex2"})
        shutil.rmtree(shares / "aa")  # the whole prefix goes
        inventory.refresh(shares, now_ns=T0 + 660 * 10**9)
        assert inventory.storage_indexes() == []

    def test_a_prefix_modified_around_its_listing_is_listed_once_more(self, tmp_path: Path):
        """A change that lands in the same second as the listing may or may
        not be in it: the prefix stays hot for one more refresh."""
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        settle(shares, T0)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 10**9)  # listed one second after the change
        walk = inventory.refresh(shares, now_ns=T0 + 300 * 10**9)
        assert walk.listed == 1
        walk = inventory.refresh(shares, now_ns=T0 + 600 * 10**9)
        assert walk.listed == 0

    def test_full_walk_relists_everything_without_a_new_generation(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0], "bbindex2": [0]})
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        walk = inventory.refresh(shares, now_ns=T0 + 360 * 10**9, full=True)
        assert (walk.mode, walk.listed) == ("full", 2)
        assert inventory.generation == 1
        assert inventory.full_walk_at_ns == T0 + 360 * 10**9

    def test_incoming_counts_bytes_only_and_is_refreshed_every_time(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        staging = shares / "incoming" / "zz" / "zzindex9"
        staging.mkdir(parents=True)
        (staging / "0").write_bytes(b"y" * 50)
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        assert (inventory.object_count, inventory.disk_used_bytes) == (1, 150)
        shutil.rmtree(shares / "incoming")
        inventory.refresh(shares, now_ns=T0 + 360 * 10**9)
        assert inventory.disk_used_bytes == 100

    def test_matches_the_full_scan(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0, 1], "abindex2": [0], "zzindex3": [2]})
        inventory = Inventory()
        inventory.refresh(shares)
        assert (inventory.storage_indexes(), inventory.disk_used_bytes) == scan_shares(shares)

    def test_missing_tree_is_empty(self, tmp_path: Path):
        inventory = Inventory()
        walk = inventory.refresh(tmp_path / "nope")
        assert (walk.prefixes, inventory.object_count, inventory.generation) == (0, 0, 0)


class TestChangesSince:
    def inventory(self) -> Inventory:
        inventory = Inventory()
        inventory.generation = 0
        return inventory

    def test_deltas_net_out_across_generations(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)  # generation 1
        add_share(shares, "bbindex2")
        inventory.refresh(shares, now_ns=T0 + 360 * 10**9)  # 2: +bbindex2
        shutil.rmtree(shares / "bb")
        add_share(shares, "ccindex3")
        inventory.refresh(shares, now_ns=T0 + 660 * 10**9)  # 3: -bbindex2 +ccindex3
        assert inventory.generation == 3
        assert inventory.changes_since(3) == (set(), set())
        assert inventory.changes_since(2) == ({"ccindex3"}, {"bbindex2"})
        assert inventory.changes_since(1) == ({"ccindex3"}, set())  # bbindex2 came and went
        assert inventory.changes_since(0) == ({"aaindex1", "ccindex3"}, set())

    def test_unknown_or_future_generation_means_full(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        inventory = Inventory()
        inventory.refresh(shares)
        assert inventory.changes_since(5) is None
        inventory.history.clear()  # a restart that lost its memory
        assert inventory.changes_since(0) is None
        assert inventory.changes_since(1) == (set(), set())

    def test_a_huge_change_is_not_kept_as_a_delta(self, tmp_path: Path, monkeypatch):
        monkeypatch.setattr("redundanet.monitor.census.HISTORY_MAX_CHANGE", 1)
        assert HISTORY_MAX_CHANGE > 1  # the module constant, untouched for other tests
        shares = make_shares(tmp_path, {"aaindex1": [0], "bbindex2": [0]})
        inventory = Inventory()
        inventory.refresh(shares)
        assert inventory.generation == 1 and not inventory.history
        assert inventory.changes_since(0) is None


class TestInventoryPersistence:
    def test_round_trip(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0, 1], "bbindex2": [0]})
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        add_share(shares, "bbindex3")
        os.utime(shares / "bb", ns=(T0 + 300 * 10**9, T0 + 300 * 10**9))  # well before the listing
        inventory.refresh(shares, now_ns=T0 + 360 * 10**9)
        restored = Inventory.from_json(json.loads(json.dumps(inventory.to_json())))
        assert restored.storage_indexes() == inventory.storage_indexes()
        assert restored.disk_used_bytes == inventory.disk_used_bytes
        assert restored.generation == 2
        assert restored.full_walk_at_ns == T0 + 60 * 10**9
        assert restored.changes_since(1) == ({"bbindex3"}, set())
        # The restored inventory picks up where the old one left: nothing to list.
        walk = restored.refresh(shares, now_ns=T0 + 660 * 10**9)
        assert (walk.mode, walk.listed) == ("incremental", 0)

    def test_rejects_other_versions_and_garbage(self):
        for data in ({"version": 99}, [], "x", {"version": 1, "prefixes": "nope"}):
            try:
                Inventory.from_json(data)
            except ValueError:
                continue
            raise AssertionError(f"accepted {data!r}")


class TestPayloads:
    def test_full_payload_has_the_list_and_the_walk(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        inventory = Inventory()
        walk = inventory.refresh(shares)
        payload = inventory_payload("n1", inventory, shares, walk, now=NOW)
        assert payload["storage_indexes"] == ["aaindex1"]
        assert payload["generation"] == 1 and payload["object_count"] == 1
        assert payload["walk"]["mode"] == "full" and payload["walk"]["listed"] == 1
        assert payload["computed_at"] == "2026-08-10T12:00:00+00:00"
        assert payload["disk_total_bytes"] is not None

    def test_delta_payload_or_none(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": [0]})
        settle(shares)
        inventory = Inventory()
        inventory.refresh(shares, now_ns=T0 + 60 * 10**9)
        add_share(shares, "bbindex2")
        inventory.refresh(shares, now_ns=T0 + 360 * 10**9)
        delta = delta_payload("n1", inventory, 1, shares, now=NOW)
        assert delta is not None
        assert (delta["base"], delta["generation"]) == (1, 2)
        assert (delta["added"], delta["removed"]) == (["bbindex2"], [])
        assert "storage_indexes" not in delta
        assert delta_payload("n1", inventory, 7, shares, now=NOW) is None


class TestDeltaCollection:
    def test_deltas_are_applied_between_full_lists(self, tmp_path: Path):
        """A node that serves generations: the first answer is the full
        list, later ones only the changes; the counts follow."""
        store = CensusStore(tmp_path / "census.sqlite")
        first = {**payload(["si1", "si2"]), "generation": 1, "computed_at": "t1"}
        collect(
            censuses({"10.100.0.10": first, "10.100.0.11": payload(["si1", "si2"])}), store=store
        )
        assert store.generation_for_address("10.100.0.10") == 1
        change = {
            "base": 1,
            "generation": 2,
            "added": ["si3"],
            "removed": ["si2"],
            "object_count": 2,
            "disk_used_bytes": 1000,
            "computed_at": "t2",
        }
        status = collect(
            censuses({"10.100.0.10": change, "10.100.0.11": payload(["si1", "si2"])}), store=store
        )
        replication = status.replication
        assert replication is not None
        assert replication.objects_total == 3  # si1, si2 (n2 only now), si3 (n1 only)
        assert replication.fully_replicated == 1
        assert replication.per_server["n1"].objects == 2
        assert store.generation_for_address("10.100.0.10") == 2

    def test_slow_census_is_shown_on_the_page(self):
        slow = {**payload(["si1"]), "fetch_seconds": 74.1, "walk": {"seconds": 2.0, "mode": "full"}}
        status = collect(censuses({"10.100.0.10": slow, "10.100.0.11": payload(["si1"])}))
        assert status.replication is not None
        assert status.replication.per_server["n1"].fetch_seconds == 74.1
        assert "census fetch 74 s" in render_html(status)
        assert "walk" not in render_html(status).split("n1")[1].split("</tr>")[0]


class TestStorageDown:
    def test_a_node_whose_tahoe_is_down_keeps_its_inventory_but_serves_nothing(self):
        """Its census answers (the sidecar is up), so placement counts its
        shares; availability does not, and the page says why."""
        status = collect_status(
            manifest(),
            "hub",
            lambda _ip: 5.0,
            storage_connected=2,
            furl_present=True,
            manifest_synced_at=NOW,
            now=NOW,
            fetch_census=censuses(
                {"10.100.0.10": payload(["si1", "si2"]), "10.100.0.11": payload(["si1", "si2"])}
            ),
            probe_storage=lambda ip: ip != "10.100.0.11",
        )
        replication = status.replication
        assert replication is not None
        assert replication.complete and replication.fully_replicated == 2
        assert replication.available_full == 0  # only n1 serves right now
        assert replication.unreadable_now == 0  # needed=1, n1 still has both
        assert replication.per_server["n2"].source == "live"
        assert replication.per_server["n2"].storage_listening is False
        assert status.overall == "degraded"
        html = render_html(status)
        assert "2/2*" not in html  # the census is complete
        assert "whose Tahoe is down" in html and "0/2 objects fully reachable" in html
