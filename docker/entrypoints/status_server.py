#!/usr/bin/env python3
"""Public network-status page for the RedundaNet hub.

Runs under supervisord next to tincd and the introducer. A background thread
collects the network status every minute (manifest + VPN pings + introducer
announcements); a stdlib HTTP server serves the cached snapshot:

    /             human status page
    /status.json  machine-readable status (alerting hook)
    /healthz      liveness for the fly.io check

A collector failure can never take the page down — the page keeps serving the
last snapshot and shows how stale it is.
"""

from __future__ import annotations

import contextlib
import json
import os
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import yaml

from redundanet.core.manifest import locate_manifest
from redundanet.monitor.census import CENSUS_PORT
from redundanet.monitor.census_store import CensusStore, unchanged_marker
from redundanet.monitor.introducer import Announcement, parse_announcements
from redundanet.monitor.render import render_html
from redundanet.monitor.status import (
    ThrottledFetcher,
    append_sample,
    collect_status,
    rollup_hours,
    uptime_stats,
    uptime_windows,
)
from redundanet.monitor.usage import USAGE_PORT
from redundanet.utils.logging import get_logger, setup_logging
from redundanet.vpn.traffic import STORAGE_TUB_PORT

MANIFEST_DIR = Path("/var/lib/redundanet/manifest")
FURL_PATH = Path("/var/lib/tahoe-introducer/private/introducer.furl")
HISTORY_PATH = Path("/var/lib/tahoe-introducer/monitor/history.jsonl")
ROLLUP_PATH = Path("/var/lib/tahoe-introducer/monitor/uptime-hourly.jsonl")
CENSUS_CACHE_DIR = Path("/var/lib/tahoe-introducer/monitor/census")
CENSUS_STORE_PATH = CENSUS_CACHE_DIR / "census.sqlite"
USAGE_CACHE_DIR = Path("/var/lib/tahoe-introducer/monitor/usage")
INTRODUCER_JSON = "http://127.0.0.1:4458/?t=json"
INTRODUCER_PAGE = "http://127.0.0.1:4458/"
INTERVAL = 60
# A census is megabytes on a big node and the node recomputes it every few
# minutes anyway; ask each node this often, not every collection.
CENSUS_REFRESH = 300
CENSUS_TIMEOUT = 20  # seconds: a 2 MB answer over a home uplink takes a few


def ping(vpn_ip: str) -> float | None:
    """RTT in ms to a VPN IP, or None if unreachable."""
    if not vpn_ip:
        return None
    try:
        result = subprocess.run(
            ["ping", "-c", "1", "-W", "2", vpn_ip], capture_output=True, text=True, timeout=5
        )
    except (subprocess.TimeoutExpired, OSError):
        return None
    if result.returncode != 0:
        return None
    for token in result.stdout.split():
        if token.startswith("time="):
            try:
                return float(token[5:])
            except ValueError:
                return None
    return 0.0


def probe_storage(vpn_ip: str, port: int = STORAGE_TUB_PORT, timeout: float = 3.0) -> bool | None:
    """Whether a storage node's Tahoe server accepts a connection on its tub
    port over the VPN. Its census sidecar answering is no proof: Tahoe can
    be down while the node and the sidecar are up."""
    if not vpn_ip:
        return None
    try:
        with socket.create_connection((vpn_ip, port), timeout=timeout):
            return True
    except OSError:
        return False


def storage_server_count() -> int | None:
    """Announced storage servers, from the introducer's JSON. None = introducer down."""
    try:
        with urllib.request.urlopen(INTRODUCER_JSON, timeout=5) as response:
            data = json.load(response)
        return int((data.get("announcement_summary") or {}).get("storage", 0))
    except Exception:
        return None


def introducer_announcements() -> list[Announcement] | None:
    """Every announcement the introducer holds, from its status page; None
    when the page cannot be read (the JSON count above is the fallback)."""
    try:
        with urllib.request.urlopen(INTRODUCER_PAGE, timeout=5) as response:
            page = response.read().decode("utf-8", "replace")
    except Exception:
        return None
    return parse_announcements(page)


class DeltaCensusFetcher:
    """Fetch a node's census as a delta when the store holds a generation
    for it (``/census?since=<generation>``: a few hundred bytes), as the
    full list otherwise, conditionally (``If-None-Match``) for nodes that
    still serve only the full list. The payload is returned for the store
    to apply, never kept here: a census is megabytes per node."""

    def __init__(self, generation_of: Callable[[str], int | None]) -> None:
        self._generation_of = generation_of
        self._etags: dict[str, str] = {}  # vpn_ip -> ETag of the full list

    def __call__(self, vpn_ip: str) -> dict | None:
        if not vpn_ip:
            return None
        since = self._generation_of(vpn_ip)
        url = f"http://{vpn_ip}:{CENSUS_PORT}/census"
        if since is not None:
            url += f"?since={since}"
        request = urllib.request.Request(url)  # noqa: S310 - http:// to a VPN address
        etag = self._etags.get(vpn_ip)
        if since is None and etag:
            request.add_header("If-None-Match", etag)
        started = time.monotonic()
        try:
            with urllib.request.urlopen(  # noqa: S310 - http:// to a VPN address built above
                request, timeout=CENSUS_TIMEOUT
            ) as response:
                payload = json.load(response)
                new_etag = response.headers.get("ETag", "")
        except urllib.error.HTTPError as e:
            if e.code == 304 and etag:
                return {
                    "node_unchanged": True,
                    "fetch_seconds": round(time.monotonic() - started, 2),
                }
            return None
        except Exception:
            return None
        if new_etag:
            self._etags[vpn_ip] = new_etag
        if isinstance(payload, dict):
            payload["fetch_seconds"] = round(time.monotonic() - started, 2)
            return payload
        return None


STORE = CensusStore(":memory:")  # main() opens the one on the volume
fetch_census = DeltaCensusFetcher(lambda vpn_ip: STORE.generation_for_address(vpn_ip))
census_fetcher = ThrottledFetcher(fetch_census, ttl=CENSUS_REFRESH, remember=unchanged_marker)


def fetch_usage(vpn_ip: str) -> dict | None:
    """A client node's /usage payload over the VPN, or None."""
    if not vpn_ip:
        return None
    try:
        with urllib.request.urlopen(f"http://{vpn_ip}:{USAGE_PORT}/usage", timeout=5) as response:
            return json.load(response)
    except Exception:
        return None


# Usage changes hourly at most (the meter's cadence); ask each client this often.
usage_fetcher = ThrottledFetcher(fetch_usage, ttl=CENSUS_REFRESH)


def manifest_synced_at() -> datetime | None:
    manifest_file = locate_manifest(MANIFEST_DIR)
    if manifest_file is None:
        return None
    fetch_head = MANIFEST_DIR / ".git" / "FETCH_HEAD"
    source = fetch_head if fetch_head.exists() else manifest_file
    return datetime.fromtimestamp(source.stat().st_mtime, tz=UTC)


class Snapshot:
    """The latest collected status, shared between collector and server."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.html = "<html><body>collecting first sample…</body></html>"
        self.json_body = b'{"overall": "starting"}'
        self.collected_at = 0.0

    def update(self, html: str, json_body: bytes) -> None:
        with self._lock:
            self.html = html
            self.json_body = json_body
            self.collected_at = time.time()

    def get(self) -> tuple[str, bytes, float]:
        with self._lock:
            return self.html, self.json_body, self.collected_at


SNAPSHOT = Snapshot()


def collect_once(node_name: str) -> None:
    logger = get_logger()
    manifest_file = locate_manifest(MANIFEST_DIR)
    manifest = {}
    if manifest_file is not None:
        manifest = yaml.safe_load(manifest_file.read_text()) or {}

    status = collect_status(
        manifest=manifest,
        self_name=node_name,
        ping=ping,
        storage_connected=storage_server_count(),
        furl_present=FURL_PATH.exists() and FURL_PATH.stat().st_size > 0,
        manifest_synced_at=manifest_synced_at(),
        fetch_census=census_fetcher,
        census_store=STORE,
        fetch_usage=usage_fetcher,
        usage_cache_dir=USAGE_CACHE_DIR,
        announcements=introducer_announcements(),
        probe_storage=probe_storage,
    )
    append_sample(HISTORY_PATH, status)
    uptimes = uptime_stats(HISTORY_PATH, timedelta(hours=24))
    # Completed hours fold into the rollup file; 7d/30d read from there.
    rollup_hours(HISTORY_PATH, ROLLUP_PATH)
    windows = uptime_windows(ROLLUP_PATH, {"7d": timedelta(days=7), "30d": timedelta(days=30)})
    for node in status.nodes:
        node.uptime_24h = uptimes.get(node.name)
        node.uptime_7d = windows["7d"].get(node.name)
        node.uptime_30d = windows["30d"].get(node.name)

    SNAPSHOT.update(
        render_html(status),
        json.dumps(status.to_dict(), indent=1).encode(),
    )
    logger.info("Status collected", overall=status.overall)


def wait_for_manifest(timeout: float = 120, poll: float = 5) -> bool:
    """Give the manifest sync time to clone on a fresh machine: a sample
    collected without a manifest shows a network of no nodes as down."""
    deadline = time.monotonic() + timeout
    while locate_manifest(MANIFEST_DIR) is None:
        if time.monotonic() >= deadline:
            return False
        time.sleep(poll)
    return True


def collector_loop(node_name: str) -> None:
    logger = get_logger()
    if not wait_for_manifest():
        logger.warning("No manifest yet; collecting anyway")
    while True:
        try:
            collect_once(node_name)
        except Exception as e:  # the page must survive any collector failure
            logger.warning("Status collection failed", error=str(e))
        time.sleep(INTERVAL)


class Handler(BaseHTTPRequestHandler):
    server_version = "redundanet-status"

    def do_GET(self) -> None:
        html, json_body, collected_at = SNAPSHOT.get()
        if self.path.startswith("/healthz"):
            body, ctype = b"ok\n", "text/plain"
        elif self.path.startswith("/status.json"):
            body, ctype = json_body, "application/json"
        elif self.path in ("/", "/index.html"):
            stale = time.time() - collected_at > 5 * INTERVAL
            if stale and collected_at:
                marker = f"<!-- stale since {datetime.fromtimestamp(collected_at, tz=UTC)} -->"
                html = html.replace("</body>", marker + "</body>")
            body, ctype = html.encode(), "text/html; charset=utf-8"
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args: object) -> None:  # quiet access log
        pass


def open_store(path: Path = CENSUS_STORE_PATH, legacy_dir: Path = CENSUS_CACHE_DIR) -> CensusStore:
    """The census store on the volume. The first time, the per-node JSON
    files an older hub cached are imported as cached inventories and
    removed: the store is the cache now."""
    global STORE
    logger = get_logger()
    path.parent.mkdir(parents=True, exist_ok=True)
    store = CensusStore(path)
    if not store.records():
        imported = store.import_legacy(legacy_dir, time.time())
        for file in imported:
            with contextlib.suppress(OSError):
                file.unlink()
        if imported:
            logger.info("Census cache files imported into the store", files=len(imported))
    STORE = store
    return store


def main() -> None:
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()
    node_name = os.environ.get("REDUNDANET_NODE_NAME", "hub")
    port = int(os.environ.get("REDUNDANET_STATUS_PORT", "8080"))
    open_store()

    threading.Thread(target=collector_loop, args=(node_name,), daemon=True).start()
    logger.info("Status server listening", port=port)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()  # noqa: S104


if __name__ == "__main__":
    main()
