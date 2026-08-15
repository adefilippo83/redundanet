#!/usr/bin/env python3
"""Share-census endpoint for storage nodes.

Serves GET /census — the list of opaque storage indexes this node holds — so
the hub's status page can compute per-object replication across the network.

SECURITY: binds the node's VPN IP only, so only authenticated mesh members can
query it. Storage indexes reveal nothing about file contents, names, or owners.
"""

from __future__ import annotations

import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from redundanet.monitor.census import CENSUS_PORT, census_payload
from redundanet.utils.logging import get_logger, setup_logging

SHARES_DIR = Path("/data/storage/shares")


class Handler(BaseHTTPRequestHandler):
    server_version = "redundanet-census"
    node_name = "storage"

    def do_GET(self) -> None:
        if not self.path.startswith("/census"):
            self.send_error(404)
            return
        body = json.dumps(census_payload(self.node_name, SHARES_DIR)).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args: object) -> None:
        pass


def main() -> None:
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()
    Handler.node_name = os.environ.get("REDUNDANET_NODE_NAME", "storage")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP", "")
    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP is required")
        raise SystemExit(1)

    # The VPN interface comes up after tinc starts; retry until we can bind.
    while True:
        try:
            server = ThreadingHTTPServer((vpn_ip, CENSUS_PORT), Handler)
            break
        except OSError:
            time.sleep(5)
    logger.info("Share census listening", address=f"{vpn_ip}:{CENSUS_PORT}")
    server.serve_forever()


if __name__ == "__main__":
    main()
