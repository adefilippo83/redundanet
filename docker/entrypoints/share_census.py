#!/usr/bin/env python3
"""Share-census endpoint for storage nodes.

Serves GET /census — the list of opaque storage indexes this node holds — so
the hub's status page can compute per-object replication across the network.

The inventory is computed on a schedule by a background thread and served
from memory, never per request: on a node with a hundred thousand objects a
walk of the shares tree takes seconds on a Pi's USB disk, longer than the
hub waits, and the hub asks every minute. Until the first walk finishes the
endpoint answers 503, which the hub treats like an unreachable node (it
keeps that node's last inventory).

Environment:
  REDUNDANET_CENSUS_INTERVAL  seconds between walks (default 300)

SECURITY: binds the node's VPN IP only, so only authenticated mesh members can
query it. Storage indexes reveal nothing about file contents, names, or owners.
"""

from __future__ import annotations

import contextlib
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from redundanet.monitor.census import CENSUS_PORT, census_payload
from redundanet.utils.logging import get_logger, setup_logging

SHARES_DIR = Path("/data/storage/shares")
DEFAULT_INTERVAL = 300


class Handler(BaseHTTPRequestHandler):
    server_version = "redundanet-census"
    latest: bytes = b""  # the last census, already serialized

    def do_GET(self) -> None:
        if not self.path.startswith("/census"):
            self.send_error(404)
            return
        body = Handler.latest
        if not body:
            self.send_error(503, "census not computed yet")
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        # A reader that gave up waiting is not our problem.
        with contextlib.suppress(BrokenPipeError, ConnectionResetError):
            self.wfile.write(body)

    def log_message(self, *args: object) -> None:
        pass


def refresh(node_name: str, shares_dir: Path = SHARES_DIR) -> dict[str, object]:
    """Walk the shares tree once and publish the result to the handler."""
    started = time.monotonic()
    payload = census_payload(node_name, shares_dir)
    Handler.latest = json.dumps(payload).encode()
    return {
        "objects": payload["object_count"],
        "disk_used_bytes": payload["disk_used_bytes"],
        "seconds": round(time.monotonic() - started, 1),
    }


def census_loop(node_name: str, interval: int, shares_dir: Path = SHARES_DIR) -> None:
    logger = get_logger()
    while True:
        try:
            logger.info("Census computed", **refresh(node_name, shares_dir))
        except Exception as e:  # the endpoint must survive a bad walk
            logger.warning("Census walk failed", error=str(e))
        time.sleep(interval)


def main() -> None:
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()
    node_name = os.environ.get("REDUNDANET_NODE_NAME", "storage")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP", "")
    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP is required")
        raise SystemExit(1)
    try:
        interval = int(os.environ.get("REDUNDANET_CENSUS_INTERVAL", "") or DEFAULT_INTERVAL)
    except ValueError:
        interval = DEFAULT_INTERVAL

    threading.Thread(
        target=census_loop, args=(node_name, interval), name="census", daemon=True
    ).start()

    # The VPN interface comes up after tinc starts; retry until we can bind.
    while True:
        try:
            server = ThreadingHTTPServer((vpn_ip, CENSUS_PORT), Handler)
            break
        except OSError:
            time.sleep(5)
    logger.info("Share census listening", address=f"{vpn_ip}:{CENSUS_PORT}", interval=interval)
    server.serve_forever()


if __name__ == "__main__":
    main()
