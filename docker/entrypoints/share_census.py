#!/usr/bin/env python3
"""Share-census endpoint for storage nodes.

Serves GET /census, the list of opaque storage indexes this node holds, so
the hub's status page can compute per-object replication across the network.
A reader that already holds a generation of the inventory asks
GET /census?since=<generation> and gets only what changed since, a few
hundred bytes on a busy node instead of megabytes; when the node can no
longer account for that generation (a restart lost its memory, or the change
was too big to keep) it answers with the full inventory, which the reader
tells apart by the ``storage_indexes`` field.

The inventory is kept incrementally (see redundanet.monitor.census): a
refresh stats the prefix directories and re-lists only those that changed,
so the cost of a tick follows the number of recent uploads, not the number
of objects on the disk. A full walk happens on the first start and then once
a day as a safety net. The inventory is persisted on the storage disk itself
(``.redundanet-census.json`` next to the shares) so a restart resumes with a
cheap incremental refresh instead of walking the whole tree cold, which on
a hundred thousand objects takes minutes and competes with Tahoe for the
disk. Until the first refresh finishes the endpoint answers 503, which the
hub treats like an unreachable node (it keeps that node's last inventory).
The same happens when the shares directory is not the disk this node was
started on (a missing mount, see redundanet.storage.disk): the census is
withheld rather than an inventory of the system disk published.

Environment:
  REDUNDANET_CENSUS_INTERVAL          seconds between refreshes (default 300)
  REDUNDANET_CENSUS_FULL_INTERVAL     seconds between full walks (default 86400)
  REDUNDANET_CENSUS_PERSIST_INTERVAL  seconds between writes of the inventory
                                      file (default 900; also written on stop)

SECURITY: binds the node's VPN IP only, so only authenticated mesh members can
query it. Storage indexes reveal nothing about file contents, names, or owners.
"""

from __future__ import annotations

import contextlib
import hashlib
import json
import os
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlsplit

from redundanet.monitor.census import (
    CENSUS_PORT,
    Inventory,
    Walk,
    delta_payload,
    inventory_payload,
)
from redundanet.storage.disk import MARKER_FILE, RECORD_FILE, explain, verify
from redundanet.utils.logging import get_logger, setup_logging

STORAGE_DIR = Path("/data/storage")
SHARES_DIR = STORAGE_DIR / "shares"
INVENTORY_FILE = STORAGE_DIR / ".redundanet-census.json"
NODE_DIR = Path("/var/lib/tahoe-storage")
DISK_MARKER = STORAGE_DIR / MARKER_FILE
DISK_RECORD = NODE_DIR / RECORD_FILE
DEFAULT_INTERVAL = 300
DEFAULT_FULL_INTERVAL = 86400
DEFAULT_PERSIST_INTERVAL = 900
# Left out of the ETag: a refresh that finds the same shares must yield the
# same tag or the 304 path never triggers.
VOLATILE_FIELDS = frozenset({"computed_at", "disk_free_bytes", "walk"})


class State:
    """The inventory and what the handler needs to answer from it."""

    def __init__(self) -> None:
        self.inventory = Inventory()
        self.lock = threading.Lock()
        self.node_name = "storage"
        self.shares_dir = SHARES_DIR
        self.last_walk: Walk | None = None
        self.persisted_generation = 0
        self.persisted_at = 0.0


STATE = State()


def reset() -> None:
    """Forget everything (tests)."""
    global STATE
    STATE = State()
    Handler.latest = b""
    Handler.etag = ""


class Handler(BaseHTTPRequestHandler):
    server_version = "redundanet-census"
    latest: bytes = b""  # the last full census, already serialized
    etag: str = ""  # digest of ``latest``; the hub sends it back in If-None-Match

    def do_GET(self) -> None:
        url = urlsplit(self.path)
        if url.path != "/census":
            self.send_error(404)
            return
        body = Handler.latest
        if not body:
            self.send_error(503, "census not computed yet")
            return
        since = _since(url.query)
        if since is not None:
            with STATE.lock:
                delta = delta_payload(
                    STATE.node_name, STATE.inventory, since, STATE.shares_dir, STATE.last_walk
                )
            if delta is not None:
                self._send(json.dumps(delta).encode())
                return
        # An unchanged inventory is the common case on an idle node, and the
        # full one is megabytes: answer 304 when the reader already has it.
        if Handler.etag and self.headers.get("If-None-Match") == Handler.etag:
            self.send_response(304)
            self.send_header("ETag", Handler.etag)
            self.end_headers()
            return
        self._send(body, etag=Handler.etag)

    def _send(self, body: bytes, etag: str = "") -> None:
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if etag:
            self.send_header("ETag", etag)
        self.end_headers()
        # A reader that gave up waiting is not our problem.
        with contextlib.suppress(BrokenPipeError, ConnectionResetError):
            self.wfile.write(body)

    def log_message(self, *args: object) -> None:
        pass


def _since(query: str) -> int | None:
    """The generation a reader holds, from ``?since=``; None when absent or
    not a non-negative integer (then the full inventory is served)."""
    values = parse_qs(query).get("since")
    if not values:
        return None
    try:
        since = int(values[-1])
    except ValueError:
        return None
    return since if since >= 0 else None


def refresh(
    node_name: str,
    shares_dir: Path = SHARES_DIR,
    full: bool = False,
    now_ns: int | None = None,
) -> dict[str, Any]:
    """Refresh the inventory and publish the result to the handler."""
    with STATE.lock:
        walk = STATE.inventory.refresh(shares_dir, now_ns=now_ns, full=full)
        STATE.node_name, STATE.shares_dir, STATE.last_walk = node_name, shares_dir, walk
        payload = inventory_payload(node_name, STATE.inventory, shares_dir, walk)
    body = json.dumps(payload).encode()
    tagged = {k: v for k, v in payload.items() if k not in VOLATILE_FIELDS}
    digest = hashlib.sha256(json.dumps(tagged, sort_keys=True).encode()).hexdigest()
    Handler.etag = f'"{digest[:32]}"'
    Handler.latest = body
    return {
        "objects": payload["object_count"],
        "disk_used_bytes": payload["disk_used_bytes"],
        "seconds": round(walk.seconds, 1),
        "mode": walk.mode,
        "prefixes_listed": walk.listed,
        "added": walk.added,
        "removed": walk.removed,
        "generation": payload["generation"],
    }


def census_tick(
    node_name: str,
    shares_dir: Path = SHARES_DIR,
    marker: Path = DISK_MARKER,
    record: Path = DISK_RECORD,
    full: bool = False,
) -> bool:
    """One refresh, unless the shares directory is not this node's disk (the
    entrypoint's check, see redundanet.storage.disk): then the published
    census is withdrawn and the endpoint answers 503, so the hub keeps the
    node's last real inventory instead of counting an empty system disk."""
    logger = get_logger()
    check = verify(marker, record)
    if check.refused:
        Handler.latest = b""
        Handler.etag = ""
        logger.error("Census withheld: " + explain(check, marker, record), status=check.status)
        return False
    logger.info("Census computed", **refresh(node_name, shares_dir, full=full))
    return True


def full_walk_due(inventory: Inventory, full_interval: int, now_ns: int | None = None) -> bool:
    now_ns = time.time_ns() if now_ns is None else now_ns
    return (
        inventory.full_walk_at_ns is None
        or now_ns - inventory.full_walk_at_ns >= full_interval * 10**9
    )


def load_inventory(path: Path = INVENTORY_FILE) -> Inventory | None:
    """The inventory a previous run persisted, or None (then the first
    refresh is a full walk)."""
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return None
    try:
        return Inventory.from_json(data)
    except ValueError:
        return None


def persist(path: Path = INVENTORY_FILE, now: float | None = None) -> bool:
    """Write the inventory next to the shares, atomically."""
    now = time.time() if now is None else now
    with STATE.lock:
        data = STATE.inventory.to_json()
        generation = STATE.inventory.generation
    tmp = path.with_name(path.name + ".tmp")
    try:
        tmp.write_text(json.dumps(data))
        tmp.replace(path)
    except OSError as e:
        get_logger().warning("Could not persist the census inventory", path=str(path), error=str(e))
        with contextlib.suppress(OSError):
            tmp.unlink()
        return False
    STATE.persisted_generation = generation
    STATE.persisted_at = now
    return True


def persist_if_due(
    path: Path = INVENTORY_FILE, interval: int = DEFAULT_PERSIST_INTERVAL, now: float | None = None
) -> bool:
    """Persist when the inventory changed since the last write and the write
    interval has passed: a busy node changes every tick, and the file is
    megabytes on a big disk."""
    now = time.time() if now is None else now
    if STATE.inventory.generation == STATE.persisted_generation:
        return False
    if now - STATE.persisted_at < interval:
        return False
    return persist(path, now)


def census_loop(
    node_name: str,
    interval: int,
    shares_dir: Path = SHARES_DIR,
    marker: Path = DISK_MARKER,
    record: Path = DISK_RECORD,
    full_interval: int = DEFAULT_FULL_INTERVAL,
    inventory_file: Path = INVENTORY_FILE,
    persist_interval: int = DEFAULT_PERSIST_INTERVAL,
) -> None:
    logger = get_logger()
    while True:
        try:
            full = full_walk_due(STATE.inventory, full_interval)
            if census_tick(node_name, shares_dir, marker, record, full=full):
                persist_if_due(inventory_file, persist_interval)
        except Exception as e:  # the endpoint must survive a bad walk
            logger.warning("Census refresh failed", error=str(e))
        time.sleep(interval)


def _int_env(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, "") or default)
    except ValueError:
        return default


def main() -> None:
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()
    node_name = os.environ.get("REDUNDANET_NODE_NAME", "storage")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP", "")
    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP is required")
        raise SystemExit(1)
    interval = _int_env("REDUNDANET_CENSUS_INTERVAL", DEFAULT_INTERVAL)
    full_interval = _int_env("REDUNDANET_CENSUS_FULL_INTERVAL", DEFAULT_FULL_INTERVAL)
    persist_interval = _int_env("REDUNDANET_CENSUS_PERSIST_INTERVAL", DEFAULT_PERSIST_INTERVAL)

    loaded = load_inventory(INVENTORY_FILE)
    if loaded is not None:
        STATE.inventory = loaded
        STATE.persisted_generation = loaded.generation
        STATE.persisted_at = time.time()
        logger.info(
            "Census inventory loaded",
            path=str(INVENTORY_FILE),
            objects=loaded.object_count,
            generation=loaded.generation,
        )

    def on_stop(_signum: int, _frame: object) -> None:
        if STATE.inventory.generation != STATE.persisted_generation:
            persist(INVENTORY_FILE)
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, on_stop)

    threading.Thread(
        target=census_loop,
        args=(node_name, interval),
        kwargs={
            "full_interval": full_interval,
            "inventory_file": INVENTORY_FILE,
            "persist_interval": persist_interval,
        },
        name="census",
        daemon=True,
    ).start()

    # The VPN interface comes up after tinc starts; retry until we can bind.
    while True:
        try:
            server = ThreadingHTTPServer((vpn_ip, CENSUS_PORT), Handler)
            break
        except OSError:
            time.sleep(5)
    logger.info(
        "Share census listening",
        address=f"{vpn_ip}:{CENSUS_PORT}",
        interval=interval,
        full_interval=full_interval,
    )
    server.serve_forever()


if __name__ == "__main__":
    main()
