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

import json
import os
import subprocess
import threading
import time
import urllib.request
from datetime import UTC, datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import yaml

from redundanet.core.manifest import locate_manifest
from redundanet.monitor.census import CENSUS_PORT
from redundanet.monitor.render import render_html
from redundanet.monitor.status import append_sample, collect_status, uptime_stats
from redundanet.utils.logging import get_logger, setup_logging

MANIFEST_DIR = Path("/var/lib/redundanet/manifest")
FURL_PATH = Path("/var/lib/tahoe-introducer/private/introducer.furl")
HISTORY_PATH = Path("/var/lib/tahoe-introducer/monitor/history.jsonl")
CENSUS_CACHE_DIR = Path("/var/lib/tahoe-introducer/monitor/census")
INTRODUCER_JSON = "http://127.0.0.1:4458/?t=json"
INTERVAL = 60


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


def storage_server_count() -> int | None:
    """Announced storage servers, from the introducer's JSON. None = introducer down."""
    try:
        with urllib.request.urlopen(INTRODUCER_JSON, timeout=5) as response:
            data = json.load(response)
        return int((data.get("announcement_summary") or {}).get("storage", 0))
    except Exception:
        return None


def fetch_census(vpn_ip: str) -> dict | None:
    """A storage node's /census payload over the VPN, or None."""
    if not vpn_ip:
        return None
    try:
        with urllib.request.urlopen(
            f"http://{vpn_ip}:{CENSUS_PORT}/census", timeout=5
        ) as response:
            return json.load(response)
    except Exception:
        return None


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
        fetch_census=fetch_census,
        census_cache_dir=CENSUS_CACHE_DIR,
    )
    append_sample(HISTORY_PATH, status)
    uptimes = uptime_stats(HISTORY_PATH, timedelta(hours=24))
    for node in status.nodes:
        node.uptime_24h = uptimes.get(node.name)

    SNAPSHOT.update(
        render_html(status),
        json.dumps(status.to_dict(), indent=1).encode(),
    )
    logger.info("Status collected", overall=status.overall)


def collector_loop(node_name: str) -> None:
    logger = get_logger()
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


def main() -> None:
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()
    node_name = os.environ.get("REDUNDANET_NODE_NAME", "hub")
    port = int(os.environ.get("REDUNDANET_STATUS_PORT", "8080"))

    threading.Thread(target=collector_loop, args=(node_name,), daemon=True).start()
    logger.info("Status server listening", port=port)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()  # noqa: S104


if __name__ == "__main__":
    main()
