"""Collect and model the network's status (hub monitor).

Pure logic: every external input (manifest, pings, introducer counts, clock)
is passed in, so the whole model is unit-testable. The status server wires in
the real sources (docker/entrypoints/status_server.py).
"""

from __future__ import annotations

import contextlib
import json
from collections.abc import Callable
from dataclasses import asdict, dataclass, field
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

# A pinger takes a VPN IP and returns the RTT in milliseconds, or None.
Pinger = Callable[[str], "float | None"]
# A census fetcher takes a VPN IP and returns the node's /census payload, or None.
CensusFetcher = Callable[[str], "dict[str, Any] | None"]

STALE_SYNC_SECONDS = 3600


@dataclass
class NodeStatus:
    """One manifest node, as seen from the hub."""

    name: str
    vpn_ip: str
    roles: list[str]
    manifest_status: str
    is_self: bool = False
    reachable: bool = False
    rtt_ms: float | None = None
    uptime_24h: float | None = None  # percent, from history samples


@dataclass
class GridStatus:
    """The Tahoe grid's replication story."""

    shares_needed: int
    shares_happy: int
    shares_total: int
    storage_expected: int  # manifest nodes with the storage role
    storage_connected: int | None  # announced to the introducer; None = unknown

    @property
    def uploads_possible(self) -> bool | None:
        if self.storage_connected is None:
            return None
        return self.storage_connected >= self.shares_happy

    @property
    def tolerable_failures(self) -> int | None:
        """How many storage servers can fail without losing NEW data."""
        if self.storage_connected is None:
            return None
        placed = min(self.shares_total, self.storage_connected)
        return max(placed - self.shares_needed, 0)


@dataclass
class ServerCensus:
    """One storage node's share census, aggregated for display.

    ``source`` records where the inventory came from: ``live`` (the node
    answered this cycle), ``cached`` (the node is unreachable; this is its
    last known inventory, ``age_seconds`` old — reliable because shares are
    immutable), or ``assumed-empty`` (the node has never reported; a brand-new
    node genuinely holds nothing).
    """

    objects: int
    disk_used_bytes: int
    source: str = "live"  # "live" | "cached" | "assumed-empty"
    age_seconds: float | None = None


@dataclass
class ReplicationStatus:
    """Per-object replication computed from the storage nodes' share censuses.

    Storage indexes are opaque — this reveals placement, not content. Two
    views are computed:

    * **Placement** (``fully_replicated`` / ``under_replicated``): how many
      distinct servers hold each object, counting offline servers via their
      last known (cached) inventory. Shares are immutable, so a stale
      inventory stays accurate.
    * **Availability** (``available_full`` / ``unreadable_now``): the same
      counts restricted to servers that answered *this cycle* — the "one more
      failure and this object is gone" signal during an outage.

    ``complete`` is True only when every storage node reported live.
    """

    objects_total: int
    target_copies: int  # hosts each object should reach: min(shares_total, storage nodes)
    fully_replicated: int
    under_replicated: int
    complete: bool
    available_full: int = 0  # objects with >= target copies reachable right now
    unreadable_now: int = 0  # objects with fewer live copies than shares_needed
    per_server: dict[str, ServerCensus] = field(default_factory=dict)


@dataclass
class NetworkStatus:
    """Everything the status page shows."""

    network_name: str
    overall: str  # "ok" | "degraded" | "down"
    generated_at: str
    nodes: list[NodeStatus]
    grid: GridStatus
    furl_present: bool
    manifest_synced_at: str | None
    replication: ReplicationStatus | None = None
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        data = asdict(self)
        data["grid"]["uploads_possible"] = self.grid.uploads_possible
        data["grid"]["tolerable_failures"] = self.grid.tolerable_failures
        if data.get("replication"):
            # Aggregates only — never the raw storage indexes.
            data["replication"]["per_server"] = {
                name: asdict(census)
                for name, census in self.replication.per_server.items()  # type: ignore[union-attr]
            }
        return data


def save_census(cache_dir: Path, node_name: str, payload: dict[str, Any], now: datetime) -> None:
    """Persist a node's live census so an outage doesn't blind the counts."""
    cache_dir.mkdir(parents=True, exist_ok=True)
    record = dict(payload)
    record["cached_at"] = now.isoformat(timespec="seconds")
    # A failed cache write must never break the live census.
    with contextlib.suppress(OSError):
        (cache_dir / f"{node_name}.json").write_text(json.dumps(record))


def load_census(
    cache_dir: Path, node_name: str, now: datetime
) -> tuple[dict[str, Any], float] | None:
    """A node's last cached census and its age in seconds, or None."""
    path = cache_dir / f"{node_name}.json"
    try:
        record = json.loads(path.read_text())
        cached_at = datetime.fromisoformat(record["cached_at"])
    except (OSError, ValueError, KeyError):
        return None
    return record, max((now - cached_at).total_seconds(), 0.0)


def _human_age(seconds: float) -> str:
    if seconds < 90:
        return f"{int(seconds)}s"
    if seconds < 5400:
        return f"{int(seconds / 60)}m"
    return f"{seconds / 3600:.1f}h"


def _collect_replication(
    nodes: list[NodeStatus],
    grid: GridStatus,
    fetch_census: CensusFetcher,
    notes: list[str],
    census_cache_dir: Path | None = None,
    now: datetime | None = None,
) -> ReplicationStatus | None:
    """Aggregate the storage nodes' share censuses into replication counts.

    Inventory precedence per node: live answer > cached last answer (shares
    are immutable, so a stale inventory stays accurate) > assumed empty for a
    node that has never reported (a brand-new node genuinely holds nothing).
    This keeps the census computable through outages; availability counts
    (live copies only) carry the actual risk signal.
    """
    now = now or datetime.now(UTC)
    storage_nodes = [n for n in nodes if "tahoe_storage" in n.roles]
    if not storage_nodes:
        return None

    placed: dict[str, int] = {}
    live_copies: dict[str, int] = {}
    per_server: dict[str, ServerCensus] = {}
    missing: list[str] = []
    for node in storage_nodes:
        payload = fetch_census(node.vpn_ip)
        source, age = "live", None
        if payload:
            if census_cache_dir is not None:
                save_census(census_cache_dir, node.name, payload, now)
        else:
            missing.append(node.name)
            cached = (
                load_census(census_cache_dir, node.name, now)
                if census_cache_dir is not None
                else None
            )
            if cached is not None:
                payload, age = cached
                source = "cached"
                notes.append(
                    f"{node.name} unreachable; using its census from {_human_age(age)} ago"
                )
            else:
                payload, source = {}, "assumed-empty"
                notes.append(f"{node.name} has never reported a census; assuming empty")

        indexes = payload.get("storage_indexes") or []
        per_server[node.name] = ServerCensus(
            objects=int(payload.get("object_count", len(indexes))),
            disk_used_bytes=int(payload.get("disk_used_bytes", 0)),
            source=source,
            age_seconds=age,
        )
        for raw_index in indexes:
            storage_index = str(raw_index)
            placed[storage_index] = placed.get(storage_index, 0) + 1
            if source == "live":
                live_copies[storage_index] = live_copies.get(storage_index, 0) + 1

    if (
        not placed
        and missing
        and all(census.source == "assumed-empty" for census in per_server.values())
    ):
        # Nothing has ever reported: there is genuinely nothing to count yet.
        return None

    target = min(grid.shares_total, len(storage_nodes))
    fully = sum(1 for count in placed.values() if count >= target)
    return ReplicationStatus(
        objects_total=len(placed),
        target_copies=target,
        fully_replicated=fully,
        under_replicated=len(placed) - fully,
        complete=not missing,
        available_full=sum(1 for si in placed if live_copies.get(si, 0) >= target),
        unreadable_now=sum(1 for si in placed if live_copies.get(si, 0) < grid.shares_needed),
        per_server=per_server,
    )


def collect_status(
    manifest: dict[str, Any],
    self_name: str,
    ping: Pinger,
    storage_connected: int | None,
    furl_present: bool,
    manifest_synced_at: datetime | None,
    now: datetime | None = None,
    fetch_census: CensusFetcher | None = None,
    census_cache_dir: Path | None = None,
) -> NetworkStatus:
    """Build the status model from the raw inputs."""
    now = now or datetime.now(UTC)
    network = manifest.get("network", {}) or {}
    tahoe = network.get("tahoe", {}) or {}
    notes: list[str] = []

    nodes: list[NodeStatus] = []
    for raw in manifest.get("nodes", []) or []:
        name = str(raw.get("name", "?"))
        vpn_ip = str(raw.get("vpn_ip") or raw.get("internal_ip") or "")
        is_self = name == self_name
        rtt = None if is_self else ping(vpn_ip)
        nodes.append(
            NodeStatus(
                name=name,
                vpn_ip=vpn_ip,
                roles=[str(r) for r in raw.get("roles", []) or []],
                manifest_status=str(raw.get("status", "unknown")),
                is_self=is_self,
                reachable=True if is_self else rtt is not None,
                rtt_ms=rtt,
            )
        )

    grid = GridStatus(
        shares_needed=int(tahoe.get("shares_needed", 3)),
        shares_happy=int(tahoe.get("shares_happy", 7)),
        shares_total=int(tahoe.get("shares_total", 10)),
        storage_expected=sum(1 for n in nodes if "tahoe_storage" in n.roles),
        storage_connected=storage_connected,
    )

    # --- overall verdict -------------------------------------------------
    overall = "ok"

    replication: ReplicationStatus | None = None
    if fetch_census is not None:
        replication = _collect_replication(
            nodes, grid, fetch_census, notes, census_cache_dir=census_cache_dir, now=now
        )
        if replication is not None and replication.complete and replication.under_replicated:
            notes.append(
                f"{replication.under_replicated} object(s) stored on fewer than "
                f"{replication.target_copies} servers — re-upload or repair them"
            )
            overall = "degraded"
        if replication is not None and replication.unreadable_now:
            notes.append(
                f"{replication.unreadable_now} object(s) currently have fewer than "
                f"{grid.shares_needed} reachable copies — unreadable until a "
                "server returns"
            )
            overall = "degraded"

    unreachable = [n for n in nodes if not n.reachable and n.manifest_status != "inactive"]
    for node in unreachable:
        notes.append(f"node {node.name} is unreachable over the VPN")

    sync_stale = manifest_synced_at is None or (now - manifest_synced_at) > timedelta(
        seconds=STALE_SYNC_SECONDS
    )
    if sync_stale:
        notes.append("manifest has not synced recently")

    if storage_connected is None:
        notes.append("introducer not answering; storage server count unknown")
        overall = "degraded"
    elif storage_connected < grid.shares_happy:
        notes.append(
            f"only {storage_connected} storage server(s) connected; "
            f"uploads need {grid.shares_happy}"
        )
        overall = "degraded"

    if unreachable or sync_stale:
        overall = "degraded"

    if not furl_present:
        notes.append("introducer FURL missing — the grid cannot operate")
        overall = "down"
    if storage_connected is not None and storage_connected < grid.shares_needed:
        notes.append(
            f"fewer storage servers ({storage_connected}) than shares_needed "
            f"({grid.shares_needed}) — data is unreadable"
        )
        overall = "down"

    return NetworkStatus(
        network_name=str(network.get("name", "redundanet")),
        overall=overall,
        generated_at=now.isoformat(timespec="seconds"),
        nodes=nodes,
        grid=grid,
        furl_present=furl_present,
        manifest_synced_at=(
            manifest_synced_at.isoformat(timespec="seconds") if manifest_synced_at else None
        ),
        replication=replication,
        notes=notes,
    )


# --- history (uptime samples on the persistent volume) ----------------------

MAX_HISTORY_LINES = 4000  # ~2.7 days at one sample/minute


def append_sample(history_path: Path, status: NetworkStatus) -> None:
    """Append a compact sample; rewrite the file when it grows too large."""
    sample = {
        "ts": status.generated_at,
        "overall": status.overall,
        "up": {n.name: n.reachable for n in status.nodes},
    }
    history_path.parent.mkdir(parents=True, exist_ok=True)
    with history_path.open("a") as f:
        f.write(json.dumps(sample) + "\n")

    try:
        lines = history_path.read_text().splitlines()
    except OSError:
        return
    if len(lines) > MAX_HISTORY_LINES:
        history_path.write_text("\n".join(lines[-MAX_HISTORY_LINES // 2 :]) + "\n")


def uptime_stats(
    history_path: Path, window: timedelta, now: datetime | None = None
) -> dict[str, float]:
    """Per-node uptime percentage over the window, from history samples."""
    now = now or datetime.now(UTC)
    cutoff = now - window
    seen: dict[str, list[bool]] = {}
    try:
        lines = history_path.read_text().splitlines()
    except OSError:
        return {}
    for line in lines:
        try:
            sample = json.loads(line)
            ts = datetime.fromisoformat(sample["ts"])
        except (ValueError, KeyError):
            continue
        if ts < cutoff:
            continue
        for name, up in (sample.get("up") or {}).items():
            seen.setdefault(name, []).append(bool(up))
    return {name: round(100.0 * sum(ups) / len(ups), 1) for name, ups in seen.items() if ups}
