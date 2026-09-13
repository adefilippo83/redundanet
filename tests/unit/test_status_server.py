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


class TestConditionalCensusFetcher:
    def test_sends_the_tag_back_and_reuses_the_payload_on_304(self, monkeypatch):
        seen: list[str | None] = []

        def urlopen(request, timeout):
            seen.append(request.get_header("If-none-match"))
            if seen[-1] == '"t1"':
                raise urllib.error.HTTPError(request.full_url, 304, "Not Modified", {}, None)
            return FakeResponse({"storage_indexes": ["si1"]}, '"t1"')

        monkeypatch.setattr(status_server.urllib.request, "urlopen", urlopen)
        fetch = status_server.ConditionalCensusFetcher()
        first = fetch("10.100.0.10")
        assert first == {"storage_indexes": ["si1"]}
        assert fetch("10.100.0.10") is first  # 304: the known payload, no parsing
        assert seen == [None, '"t1"']

    def test_errors_are_none_and_never_poison_the_cache(self, monkeypatch):
        calls = {"n": 0}

        def urlopen(request, timeout):
            calls["n"] += 1
            if calls["n"] == 1:
                return FakeResponse({"storage_indexes": []}, '"t1"')
            raise OSError("unreachable")

        monkeypatch.setattr(status_server.urllib.request, "urlopen", urlopen)
        fetch = status_server.ConditionalCensusFetcher()
        assert fetch("10.100.0.10") == {"storage_indexes": []}
        assert fetch("10.100.0.10") is None  # a real failure is reported as such
        assert fetch("") is None
