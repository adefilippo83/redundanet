"""Unit tests for the hub's census store (redundanet.monitor.census_store)."""

from __future__ import annotations

import json
import threading
from pathlib import Path

from redundanet.monitor.census_store import CensusStore, unchanged_marker


def full(indexes: list[str], generation: int | None = None, computed_at: str = "t1", **extra):
    payload = {
        "object_count": len(indexes),
        "storage_indexes": indexes,
        "disk_used_bytes": 1000,
        "disk_total_bytes": 5000,
        "disk_free_bytes": 4000,
        "computed_at": computed_at,
        **extra,
    }
    if generation is not None:
        payload["generation"] = generation
    return payload


def delta(base: int, generation: int, added=(), removed=(), count: int = 0):
    return {
        "base": base,
        "generation": generation,
        "added": list(added),
        "removed": list(removed),
        "object_count": count,
        "disk_used_bytes": 0,
        "computed_at": "t2",
    }


class TestIngest:
    def test_full_lists_then_a_summary(self):
        store = CensusStore()
        assert store.ingest("n1", "10.0.0.1", full(["a", "b"]), 100.0) == "full"
        assert store.ingest("n2", "10.0.0.2", full(["a"]), 100.0) == "full"
        summary = store.summary(target=2, needed=1)
        assert (
            summary.objects_total,
            summary.fully_replicated,
            summary.available_full,
            summary.unreadable_now,
        ) == (2, 1, 1, 0)
        record = store.node("n1")
        assert record is not None
        assert (record.object_count, record.disk_used_bytes, record.disk_total_bytes) == (
            2,
            1000,
            5000,
        )

    def test_the_same_full_payload_again_changes_nothing(self):
        store = CensusStore()
        payload = full(["a"], generation=3)
        assert store.ingest("n1", "ip", payload, 1.0) == "full"
        assert store.ingest("n1", "ip", payload, 2.0) == "unchanged"
        record = store.node("n1")
        assert record is not None and record.fetched_at == 2.0
        assert store.ingest("n1", "ip", full(["a", "b"], generation=4), 3.0) == "full"
        assert store.rows() == 2

    def test_a_delta_is_applied_or_ignored(self):
        store = CensusStore()
        store.ingest("n1", "ip", full(["a", "b"], generation=1), 1.0)
        assert store.generation_for_address("ip") == 1
        change = delta(1, 2, added=["c"], removed=["a"], count=2)
        assert store.ingest("n1", "ip", change, 2.0) == "delta"
        assert store.summary(1, 1).objects_total == 2
        assert store.generation_for_address("ip") == 2
        # Against a generation this store does not hold: ignored, and the
        # next fetch asks for the full list.
        assert store.ingest("n1", "ip", delta(7, 8, added=["z"]), 3.0) == "ignored"
        assert store.generation_for_address("ip") is None
        assert store.summary(1, 1).objects_total == 2

    def test_an_empty_delta_keeps_the_cached_summary(self):
        store = CensusStore()
        store.ingest("n1", "ip", full(["a"], generation=1), 1.0)
        first = store.summary(1, 1)
        assert store.ingest("n1", "ip", delta(1, 1), 2.0) == "delta"
        assert store.summary(1, 1) is first

    def test_unchanged_marker_marks_live_and_keeps_rows(self):
        store = CensusStore()
        store.ingest("n1", "ip", full(["a"]), 1.0)
        assert store.mark_unreachable("n1", 5.0) == 4.0
        record = store.node("n1")
        assert record is not None and not record.live
        assert store.ingest("n1", "ip", unchanged_marker({}), 6.0) == "unchanged"
        record = store.node("n1")
        assert record is not None and record.live and store.rows() == 1

    def test_a_node_without_generations_always_gets_the_full_list(self):
        store = CensusStore()
        store.ingest("n1", "ip", full(["a"]), 1.0)
        assert store.generation_for_address("ip") is None
        assert store.generation_for_address("unknown") is None

    def test_fetch_and_walk_timings_are_kept(self):
        store = CensusStore()
        payload = full(
            ["a"], generation=1, fetch_seconds=74.1, walk={"seconds": 3.6, "mode": "full"}
        )
        store.ingest("n1", "ip", payload, 1.0)
        record = store.node("n1")
        assert record is not None
        assert (record.fetch_seconds, record.walk_seconds, record.walk_mode) == (
            74.1,
            3.6,
            "full",
        )
        store.ingest("n1", "ip", unchanged_marker({}), 2.0)
        record = store.node("n1")
        assert record is not None and record.fetch_seconds == 74.1

    def test_garbage_is_ignored(self):
        store = CensusStore()
        assert store.ingest("n1", "ip", {"hello": 1}, 1.0) == "ignored"
        assert store.rows() == 0


class TestAvailability:
    def test_placement_counts_everyone_and_availability_only_the_live(self):
        store = CensusStore()
        store.ingest("n1", "ip1", full(["a", "b"]), 1.0)
        store.ingest("n2", "ip2", full(["a", "b"]), 1.0)
        store.ingest("n3", "ip3", full(["c"]), 1.0)
        store.mark_unreachable("n2", 2.0)
        summary = store.summary(target=2, needed=1)
        assert (
            summary.objects_total,
            summary.fully_replicated,
            summary.available_full,
            summary.unreadable_now,
        ) == (3, 2, 0, 0)
        store.mark_unreachable("n3", 2.0)
        assert store.summary(2, 1).unreadable_now == 1  # c lives only on n3

    def test_mark_unreachable_without_a_record(self):
        assert CensusStore().mark_unreachable("ghost", 1.0) is None


class TestPrune:
    def test_forgets_nodes_that_left_the_manifest(self):
        store = CensusStore()
        store.ingest("n1", "ip1", full(["a"]), 1.0)
        store.ingest("old", "ip9", full(["z"]), 1.0)
        assert store.prune(["n1"]) == 1
        assert store.records().keys() == {"n1"} and store.rows() == 1
        assert store.prune(["n1"]) == 0


class TestPersistence:
    def test_survives_a_reopen(self, tmp_path: Path):
        path = tmp_path / "census.sqlite"
        store = CensusStore(path)
        store.ingest("n1", "ip", full(["a", "b"], generation=4), 1.0)
        store.close()
        again = CensusStore(path)
        assert again.rows() == 2 and again.generation_for_address("ip") == 4

    def test_imports_the_old_cache_files_as_cached_inventories(self, tmp_path: Path):
        (tmp_path / "n1.json").write_text(
            json.dumps(
                {
                    "node": "n1",
                    "storage_indexes": ["a"],
                    "object_count": 1,
                    "disk_used_bytes": 5,
                    "cached_at": "2026-08-10T12:00:00+00:00",
                }
            )
        )
        (tmp_path / "bad.json").write_text("{nope")
        store = CensusStore()
        imported = store.import_legacy(tmp_path, 0.0)
        assert [p.name for p in imported] == ["n1.json"]
        record = store.node("n1")
        assert record is not None and not record.live and record.object_count == 1
        assert store.summary(1, 1).objects_total == 1
        assert store.import_legacy(tmp_path / "missing", 0.0) == []


class TestThreads:
    def test_usable_from_another_thread_than_the_one_that_opened_it(self, tmp_path: Path):
        """The hub opens the store at start and uses it from its collector
        thread; SQLite refuses that unless told otherwise."""
        store = CensusStore(tmp_path / "census.sqlite")
        outcome: list[object] = []

        def collector() -> None:
            try:
                store.ingest("n1", "ip", full(["a"]), 1.0)
                outcome.append(store.summary(1, 1).objects_total)
            except Exception as e:
                outcome.append(e)

        thread = threading.Thread(target=collector)
        thread.start()
        thread.join()
        assert outcome == [1]
