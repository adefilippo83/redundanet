"""Unit tests for the hub entrypoint's fetchers (docker/entrypoints/status_server.py)."""

from __future__ import annotations

import io
import json
import sys
import threading
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import status_server  # noqa: E402


class FakeResponse(io.BytesIO):
    def __init__(self, payload: dict, etag: str):
        super().__init__(json.dumps(payload).encode())
        self.headers = {"ETag": etag}

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class TestDeltaCensusFetcher:
    def test_asks_for_a_delta_when_a_generation_is_held(self, monkeypatch):
        urls: list[str] = []

        def urlopen(request, timeout):
            urls.append(request.full_url)
            return FakeResponse({"base": 3, "generation": 4, "added": ["si2"], "removed": []}, "")

        monkeypatch.setattr(status_server.urllib.request, "urlopen", urlopen)
        fetch = status_server.DeltaCensusFetcher(lambda _ip: 3)
        delta = fetch("10.100.0.10")
        assert urls == ["http://10.100.0.10:3459/census?since=3"]
        assert delta is not None and delta["added"] == ["si2"] and "fetch_seconds" in delta

    def test_full_list_is_conditional_when_no_generation_is_held(self, monkeypatch):
        seen: list[tuple[str, str | None]] = []

        def urlopen(request, timeout):
            seen.append((request.full_url, request.get_header("If-none-match")))
            if seen[-1][1] == '"t1"':
                raise urllib.error.HTTPError(request.full_url, 304, "Not Modified", {}, None)
            return FakeResponse({"storage_indexes": ["si1"]}, '"t1"')

        monkeypatch.setattr(status_server.urllib.request, "urlopen", urlopen)
        fetch = status_server.DeltaCensusFetcher(lambda _ip: None)
        first = fetch("10.100.0.10")
        assert first is not None and first["storage_indexes"] == ["si1"]
        second = fetch("10.100.0.10")
        assert second is not None and second["node_unchanged"] is True
        assert seen == [
            ("http://10.100.0.10:3459/census", None),
            ("http://10.100.0.10:3459/census", '"t1"'),
        ]

    def test_errors_are_none(self, monkeypatch):
        def urlopen(request, timeout):
            raise OSError("unreachable")

        monkeypatch.setattr(status_server.urllib.request, "urlopen", urlopen)
        fetch = status_server.DeltaCensusFetcher(lambda _ip: None)
        assert fetch("10.100.0.10") is None
        assert fetch("") is None


class TestOpenStore:
    def test_imports_and_removes_the_old_cache_files(self, tmp_path: Path):
        (tmp_path / "n1.json").write_text(
            json.dumps(
                {
                    "node": "n1",
                    "storage_indexes": ["si1"],
                    "object_count": 1,
                    "disk_used_bytes": 1,
                    "cached_at": "2026-08-10T12:00:00+00:00",
                }
            )
        )
        store = status_server.open_store(tmp_path / "census.sqlite", legacy_dir=tmp_path)
        assert store is status_server.STORE
        assert store.rows() == 1 and not (tmp_path / "n1.json").exists()
        assert status_server.fetch_census("") is None


class TestProbeStorage:
    def test_open_closed_and_unknown(self):
        import socket

        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        port = listener.getsockname()[1]
        try:
            assert status_server.probe_storage("127.0.0.1", port=port, timeout=2) is True
        finally:
            listener.close()
        assert status_server.probe_storage("127.0.0.1", port=port, timeout=2) is False
        assert status_server.probe_storage("", port=port) is None


class TestV3Store:
    def test_put_get_and_authorization(self, tmp_path: Path):
        store = status_server.V3Store(tmp_path / "v3", "tok")
        assert store.enabled
        assert store.get("index.html") is None
        store.put("index.html", b"<html></html>")
        got = store.get("index.html")
        assert got is not None and got[0] == b"<html></html>" and got[1] > 0
        assert store.authorized("Bearer tok")
        assert not store.authorized("Bearer nope")
        assert not store.authorized("Basic tok")
        assert not store.authorized(None)
        assert store.get("../etc/passwd") is None
        try:
            store.put("other.txt", b"x")
        except KeyError:
            pass
        else:
            raise AssertionError("an unlisted name was stored")

    def test_without_a_token_nothing_is_authorized(self, tmp_path: Path):
        store = status_server.V3Store(tmp_path, None)
        assert not store.enabled and not store.authorized("Bearer ")
        assert not status_server.V3Store(tmp_path, "").enabled


class TestV3Routes:
    def _serve(self, monkeypatch, tmp_path: Path, token: str | None):
        monkeypatch.setattr(status_server, "V3", status_server.V3Store(tmp_path / "v3", token))
        server = ThreadingHTTPServer(("127.0.0.1", 0), status_server.Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        base = f"http://127.0.0.1:{server.server_port}"
        return server, base

    @staticmethod
    def _call(
        url: str,
        method: str = "GET",
        body: bytes | None = None,
        token: str | None = None,
        headers: dict[str, str] | None = None,
    ):
        request = urllib.request.Request(url, data=body, method=method)  # noqa: S310
        if token is not None:
            request.add_header("Authorization", f"Bearer {token}")
        if body is not None:
            request.add_header("Content-Type", "application/octet-stream")
        for key, value in (headers or {}).items():
            request.add_header(key, value)
        try:
            with urllib.request.urlopen(request, timeout=5) as response:  # noqa: S310
                return response.status, dict(response.headers), response.read()
        except urllib.error.HTTPError as err:
            return err.code, dict(err.headers), err.read()

    def test_publish_and_serve(self, monkeypatch, tmp_path: Path):
        server, base = self._serve(monkeypatch, tmp_path, "tok")
        try:
            assert self._call(f"{base}/v3/")[0] == 404
            assert self._call(f"{base}/v3/status.json", "PUT", b"{}", None)[0] == 401
            assert self._call(f"{base}/v3/status.json", "PUT", b"{}", "wrong")[0] == 401
            assert self._call(f"{base}/v3/other", "PUT", b"{}", "tok")[0] == 404
            assert self._call(f"{base}/v3/status.json", "PUT", b"not json", "tok")[0] == 400
            # The length guard answers before reading the body: a small body
            # with a lying Content-Length gets the 413 without a broken pipe.
            too_big = {"Content-Length": str(status_server.V3_MAX_BYTES + 1)}
            assert self._call(f"{base}/v3/status.json", "PUT", b"{}", "tok", too_big)[0] == 413
            assert (
                self._call(f"{base}/v3/status.json", "PUT", b'{"network": "demo"}', "tok")[0] == 204
            )
            page = b'<!doctype html><body><a href="status.json">status.json</a></body>'
            assert self._call(f"{base}/v3/index.html", "PUT", page, "tok")[0] == 204

            status, headers, body = self._call(f"{base}/v3/status.json")
            assert (
                status == 200
                and headers["Content-Type"] == "application/json"
                and json.loads(body) == {"network": "demo"}
            )
            assert headers["X-Published-At"].startswith("20")
            for path in ("/v3", "/v3/", "/v3/index.html"):
                status, headers, body = self._call(base + path)
                assert (
                    status == 200
                    and body == page
                    and headers["Content-Type"].startswith("text/html")
                )
                assert headers["Content-Security-Policy"].startswith("default-src 'none'")
            assert self._call(f"{base}/v3/../status.json")[0] in (200, 404)  # never the v3 store
            assert self._call(f"{base}/healthz")[0] == 200
            assert (tmp_path / "v3" / "status.json").read_bytes() == b'{"network": "demo"}'
        finally:
            server.shutdown()

    def test_publishing_is_off_without_a_token(self, monkeypatch, tmp_path: Path):
        server, base = self._serve(monkeypatch, tmp_path, None)
        try:
            assert self._call(f"{base}/v3/status.json", "PUT", b"{}", "tok")[0] == 503
            assert self._call(f"{base}/v3/")[0] == 404
        finally:
            server.shutdown()
