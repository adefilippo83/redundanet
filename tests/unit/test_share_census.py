"""Unit tests for the share census sidecar (docker/entrypoints/share_census.py)."""

from __future__ import annotations

import json
import sys
from http.client import HTTPConnection
from http.server import ThreadingHTTPServer
from pathlib import Path
from socketserver import TCPServer
from threading import Thread

import pytest

from redundanet.storage.disk import MARKER_FILE, RECORD_FILE, establish

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import share_census  # noqa: E402


@pytest.fixture(autouse=True)
def fresh_state():
    share_census.reset()
    yield
    share_census.reset()


def make_shares(tmp_path: Path, indexes: dict[str, int]) -> Path:
    shares = tmp_path / "shares"
    for si, size in indexes.items():
        si_dir = shares / si[:2] / si
        si_dir.mkdir(parents=True)
        (si_dir / "0").write_bytes(b"x" * size)
    return shares


class TestRefresh:
    def test_publishes_serialized_census(self, tmp_path: Path):
        share_census.Handler.latest = b""
        shares = make_shares(tmp_path, {"aaindex1": 100, "bbindex2": 50})
        stats = share_census.refresh("n1", shares)
        assert stats["objects"] == 2 and stats["disk_used_bytes"] == 150
        payload = json.loads(share_census.Handler.latest)
        assert payload["node"] == "n1"
        assert payload["storage_indexes"] == ["aaindex1", "bbindex2"]
        assert "computed_at" in payload


class QuietServer(ThreadingHTTPServer):
    """HTTPServer.server_bind reverse-resolves the bind address, which can
    stall for half a minute on a laptop; the tests do not need the name."""

    def server_bind(self) -> None:
        TCPServer.server_bind(self)
        self.server_name = "localhost"
        self.server_port = self.server_address[1]


class TestHandler:
    def serve(self):
        server = QuietServer(("127.0.0.1", 0), share_census.Handler)
        Thread(target=server.serve_forever, daemon=True).start()
        return server

    def get(self, server, path="/census", headers=None):
        conn = HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
        conn.request("GET", path, headers=headers or {})
        response = conn.getresponse()
        body = response.read()
        etag = response.getheader("ETag")
        conn.close()
        return response.status, body, etag

    def test_503_until_the_first_walk_is_done(self):
        share_census.Handler.latest = b""
        server = self.serve()
        try:
            assert self.get(server)[0] == 503
        finally:
            server.shutdown()

    def test_serves_the_cached_census_without_walking(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10})
        share_census.refresh("n1", shares)
        (shares / "aa" / "aaindex1" / "0").unlink()  # disk changed, cache did not
        server = self.serve()
        try:
            status, body, etag = self.get(server)
            assert status == 200
            assert json.loads(body)["storage_indexes"] == ["aaindex1"]
            assert self.get(server, "/other")[0] == 404
            # the hub sends the tag back: unchanged inventory -> 304, no body
            status, body, etag2 = self.get(server, headers={"If-None-Match": etag})
            assert (status, body, etag2) == (304, b"", etag)
            assert self.get(server, headers={"If-None-Match": '"stale"'})[0] == 200
        finally:
            server.shutdown()

    def test_etag_ignores_the_timestamp_but_not_the_inventory(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10})
        share_census.refresh("n1", shares)
        first = share_census.Handler.etag
        share_census.refresh("n1", shares)  # a later walk, same shares
        assert share_census.Handler.etag == first
        (shares / "bb" / "bbindex2").mkdir(parents=True)
        (shares / "bb" / "bbindex2" / "0").write_bytes(b"y")
        share_census.refresh("n1", shares)
        assert share_census.Handler.etag != first


class TestCensusTick:
    def test_withholds_the_census_when_the_disk_is_not_the_nodes(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10})
        marker, record = shares.parent / MARKER_FILE, tmp_path / "node" / RECORD_FILE
        establish(marker, record)
        assert share_census.census_tick("n1", shares, marker, record) is True
        assert share_census.Handler.latest
        marker.unlink()  # the disk went away: an empty directory under the mountpoint
        assert share_census.census_tick("n1", shares, marker, record) is False
        assert share_census.Handler.latest == b"" and share_census.Handler.etag == ""

    def test_walks_before_the_node_marked_its_disk(self, tmp_path: Path):
        share_census.Handler.latest = b""
        shares = make_shares(tmp_path, {"aaindex1": 10})
        marker, record = shares.parent / MARKER_FILE, tmp_path / "node" / RECORD_FILE
        assert share_census.census_tick("n1", shares, marker, record) is True
        assert json.loads(share_census.Handler.latest)["storage_indexes"] == ["aaindex1"]


class TestIncrementalRefresh:
    def test_second_refresh_is_incremental(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10, "bbindex2": 20})
        first = share_census.refresh("n1", shares)
        assert (first["mode"], first["prefixes_listed"], first["generation"]) == ("full", 2, 1)
        second = share_census.refresh("n1", shares, now_ns=first_listing_plus(300))
        assert (second["mode"], second["generation"]) == ("incremental", 1)
        (shares / "cc" / "ccindex3").mkdir(parents=True)
        (shares / "cc" / "ccindex3" / "0").write_bytes(b"z")
        third = share_census.refresh("n1", shares, now_ns=first_listing_plus(600))
        assert (third["prefixes_listed"], third["added"], third["generation"]) == (1, 1, 2)
        assert json.loads(share_census.Handler.latest)["walk"]["mode"] == "incremental"

    def test_full_walk_due(self):
        inventory = share_census.Inventory()
        assert share_census.full_walk_due(inventory, 86400, now_ns=10**18)
        inventory.full_walk_at_ns = 10**18
        assert not share_census.full_walk_due(inventory, 86400, now_ns=10**18 + 3600 * 10**9)
        assert share_census.full_walk_due(inventory, 86400, now_ns=10**18 + 86400 * 10**9)


def first_listing_plus(seconds: int) -> int:
    """A clock well after the tree was made, so unchanged prefixes are cold."""
    import time

    return time.time_ns() + seconds * 10**9


class TestDeltaEndpoint(TestHandler):
    def test_since_serves_only_the_changes(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10})
        share_census.refresh("n1", shares)
        (shares / "bb" / "bbindex2").mkdir(parents=True)
        (shares / "bb" / "bbindex2" / "0").write_bytes(b"y")
        share_census.refresh("n1", shares, now_ns=first_listing_plus(300))
        server = self.serve()
        try:
            status, body, etag = self.get(server, "/census?since=1")
            delta = json.loads(body)
            assert status == 200 and etag is None
            assert (delta["base"], delta["generation"]) == (1, 2)
            assert (delta["added"], delta["removed"]) == (["bbindex2"], [])
            assert "storage_indexes" not in delta
            # already current: an empty delta, still cheap
            status, body, _ = self.get(server, "/census?since=2")
            assert status == 200 and json.loads(body)["added"] == []
            # a generation the node cannot account for: the full inventory
            status, body, etag = self.get(server, "/census?since=9")
            assert status == 200 and etag
            assert json.loads(body)["storage_indexes"] == ["aaindex1", "bbindex2"]
            # garbage is ignored, not an error
            assert json.loads(self.get(server, "/census?since=x")[1])["storage_indexes"]
        finally:
            server.shutdown()


class TestPersistence:
    def test_persist_and_load(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10, "bbindex2": 20})
        share_census.refresh("n1", shares)
        path = tmp_path / "census.json"
        assert share_census.persist(path, now=1000.0)
        assert not (tmp_path / "census.json.tmp").exists()
        loaded = share_census.load_inventory(path)
        assert loaded is not None
        assert loaded.storage_indexes() == ["aaindex1", "bbindex2"]
        assert loaded.generation == 1
        assert share_census.STATE.persisted_generation == 1

    def test_persist_if_due_waits_for_a_change_and_the_interval(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10})
        share_census.refresh("n1", shares)
        path = tmp_path / "census.json"
        assert share_census.persist_if_due(path, interval=900, now=1000.0)  # first write
        assert not share_census.persist_if_due(path, interval=900, now=2000.0)  # no change
        (shares / "bb" / "bbindex2").mkdir(parents=True)
        (shares / "bb" / "bbindex2" / "0").write_bytes(b"y")
        share_census.refresh("n1", shares, now_ns=first_listing_plus(300))
        assert not share_census.persist_if_due(path, interval=900, now=1500.0)  # too soon
        assert share_census.persist_if_due(path, interval=900, now=1901.0)
        loaded = share_census.load_inventory(path)
        assert loaded is not None and loaded.generation == 2

    def test_load_tolerates_missing_or_broken_files(self, tmp_path: Path):
        assert share_census.load_inventory(tmp_path / "none.json") is None
        (tmp_path / "bad.json").write_text("{not json")
        assert share_census.load_inventory(tmp_path / "bad.json") is None
        (tmp_path / "old.json").write_text(json.dumps({"version": 0}))
        assert share_census.load_inventory(tmp_path / "old.json") is None

    def test_persist_reports_an_unwritable_path(self, tmp_path: Path):
        shares = make_shares(tmp_path, {"aaindex1": 10})
        share_census.refresh("n1", shares)
        assert not share_census.persist(tmp_path / "missing-dir" / "census.json")
