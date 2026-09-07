#!/usr/bin/env python3
"""Usage meter for the tahoe-client container.

Measures what this node's member occupies on the grid (every file reachable
from the client's aliases, each weighted by its own erasure coding) against
the member's allocation from the manifest, and publishes the result:

  * GET /usage on the node's VPN address (port 3460): the hub aggregates the
    reports per member for the status page. Served from the last computed
    payload, never computed per request.
  * /var/lib/tahoe-client/redundanet-usage.json: read by the local
    enforcement points (backup-sync, `redundanet storage upload`).

`--once` computes now, prints the JSON and exits (used by `redundanet storage
quota`).

Environment:
  REDUNDANET_NODE_NAME, REDUNDANET_INTERNAL_VPN_IP
  REDUNDANET_USAGE_INTERVAL   seconds between measurements (default 900)
  REDUNDANET_QUOTA_ENFORCE    "true"/"false" overrides the manifest's setting
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from redundanet.core.manifest import read_manifest
from redundanet.monitor.usage import USAGE_FILE, USAGE_PORT, usage_payload, write_usage_file
from redundanet.storage.inventory import all_file_caps, grid_footprint

NODE_DIR = "/var/lib/tahoe-client"
MANIFEST_DIR = Path("/var/lib/redundanet/manifest")
STARTUP_DELAY = 120


def log(message: str) -> None:
    print(f"usage-meter: {message}", flush=True)


def run_tahoe(args: list[str], timeout: int = 3600) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["tahoe", "-d", NODE_DIR, *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def load_manifest() -> dict[str, Any]:
    manifest = read_manifest(MANIFEST_DIR)
    if not manifest:
        log("no readable manifest; allocation unknown (0)")
    return manifest


def enforce_override(environ: dict[str, str]) -> bool | None:
    raw = environ.get("REDUNDANET_QUOTA_ENFORCE", "").strip().lower()
    if raw in ("true", "1", "yes"):
        return True
    if raw in ("false", "0", "no"):
        return False
    return None


def measure(node_name: str, environ: dict[str, str], run=run_tahoe) -> dict[str, Any]:
    footprint = grid_footprint(all_file_caps(run, log=log))
    payload = usage_payload(
        node_name, load_manifest(), footprint, enforce_override=enforce_override(environ)
    )
    try:
        write_usage_file(payload, USAGE_FILE)
    except OSError as e:
        log(f"cannot write {USAGE_FILE}: {e}")
    return payload


class Handler(BaseHTTPRequestHandler):
    server_version = "redundanet-usage"
    latest: dict[str, Any] = {}

    def do_GET(self) -> None:
        if not self.path.startswith("/usage") or not Handler.latest:
            self.send_error(404)
            return
        body = json.dumps(Handler.latest).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args: object) -> None:
        pass


def meter_loop(node_name: str, interval: int) -> None:
    time.sleep(STARTUP_DELAY)
    while True:
        try:
            payload = measure(node_name, dict(os.environ))
            Handler.latest = payload
            log(
                f"{payload['member']}: {payload['used_bytes']} of {payload['allocation_bytes']} "
                f"bytes used ({payload['files']} files, {payload['encoding']}, "
                f"enforce={payload['enforce']})"
            )
        except subprocess.TimeoutExpired:
            log("a tahoe command timed out; will retry next cycle")
        except Exception as e:  # the meter must survive anything transient
            log(f"measurement failed (will retry next cycle): {e}")
        time.sleep(interval)


def main() -> int:
    node_name = os.environ.get("REDUNDANET_NODE_NAME", "")
    if not node_name:
        log("REDUNDANET_NODE_NAME is required")
        return 1

    if "--once" in sys.argv:
        print(json.dumps(measure(node_name, dict(os.environ)), indent=1))
        return 0

    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP", "")
    if not vpn_ip:
        log("REDUNDANET_INTERNAL_VPN_IP is required")
        return 1
    try:
        interval = int(os.environ.get("REDUNDANET_USAGE_INTERVAL", "900"))
    except ValueError:
        interval = 900

    threading.Thread(target=meter_loop, args=(node_name, interval), daemon=True).start()
    # The VPN interface comes up after tinc starts; retry until we can bind.
    while True:
        try:
            server = ThreadingHTTPServer((vpn_ip, USAGE_PORT), Handler)
            break
        except OSError:
            time.sleep(5)
    log(f"serving /usage on {vpn_ip}:{USAGE_PORT}, measuring every {interval}s")
    server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
