"""Unit tests for the share census sidecar (docker/entrypoints/share_census.py)."""

from __future__ import annotations

import json
import sys
from http.client import HTTPConnection
from http.server import ThreadingHTTPServer
from pathlib import Path
from socketserver import TCPServer
from threading import Thread

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import share_census  # noqa: E402


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

    def get(self, server, path="/census"):
        conn = HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
        conn.request("GET", path)
        response = conn.getresponse()
        body = response.read()
        conn.close()
        return response.status, body

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
            status, body = self.get(server)
            assert status == 200
            assert json.loads(body)["storage_indexes"] == ["aaindex1"]
            assert self.get(server, "/other")[0] == 404
        finally:
            server.shutdown()
