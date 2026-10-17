"""Unit tests for the hub entrypoint's fetchers (docker/entrypoints/status_server.py)."""

from __future__ import annotations

import io
import json
import sys
import urllib.error
import urllib.request
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
