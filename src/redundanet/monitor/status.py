"""Collect and model the network's status (hub monitor).

Pure logic: every external input (manifest, pings, introducer counts, clock)
is passed in, so the whole model is unit-testable. The status server wires in
the real sources (docker/entrypoints/status_server.py).
"""

from __future__ import annotations

import contextlib
import json
import time
from collections.abc import Callable
from dataclasses import asdict, dataclass, field
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from redundanet.core.quota import MemberQuota, compute_quotas, format_size, report_age
from redundanet.monitor.census_store import CensusStore
from redundanet.monitor.introducer import Announcement, identity_notes, summarize_storage
from redundanet.vpn.traffic import STORAGE_TUB_PORT

# A pinger takes a VPN IP and returns the RTT in milliseconds, or None.
Pinger = Callable[[str], "float | None"]
# A census fetcher takes a VPN IP and returns the node's /census payload, or None.
CensusFetcher = Callable[[str], "dict[str, Any] | None"]
# A usage fetcher takes a VPN IP and returns a client node's /usage payload, or None.
UsageFetcher = Callable[[str], "dict[str, Any] | None"]
# A storage prober takes a VPN IP and says whether the node's Tahoe storage
# server accepts connections on its tub port; None when it could not probe.
StorageProber = Callable[[str], "bool | None"]
# A usage report older than this gets a note: the node's meter is not reporting.
STALE_USAGE_AFTER = 6 * 3600


class ThrottledFetcher:
    """Wrap a fetcher so each address is asked again only after ``ttl`` seconds.

    The collector runs every minute, but a census is a few megabytes per
    storage node and the node itself only recomputes it every few minutes, so
    fetching it per collection wastes the VPN and the hub. In between, the
    last answer is returned as if fetched: it is at most ``ttl`` old, and
    shares are immutable, so it stays accurate. A failed fetch is not cached:
    the next collection asks again, so an outage is seen within a minute.
    """

    def __init__(
        self,
        fetch: CensusFetcher,
        ttl: float,
        clock: Callable[[], float] | None = None,
        remember: Callable[[dict[str, Any]], dict[str, Any]] | None = None,
    ) -> None:
        self._fetch = fetch
        self._ttl = ttl
        self._clock = clock or time.monotonic
        # What a hit returns: the payload itself, or what ``remember`` makes
        # of it (a census is megabytes and lives in the store once applied;
        # a marker is enough to say the node is still there).
        self._remember = remember
        self._last: dict[str, tuple[float, dict[str, Any]]] = {}

    def __call__(self, address: str) -> dict[str, Any] | None:
        now = self._clock()
        hit = self._last.get(address)
        if hit is not None and now - hit[0] < self._ttl:
            return hit[1]
        payload = self._fetch(address)
        if payload:
            self._last[address] = (now, self._remember(payload) if self._remember else payload)
        return payload


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
    # Storage nodes only: whether Tahoe answers on its tub port (None: not
    # probed, the node being unreachable or the hub not probing).
    storage_listening: bool | None = None
    uptime_24h: float | None = None  # percent, from history samples
    uptime_7d: float | None = None  # percent, from hourly rollups
    uptime_30d: float | None = None


@dataclass
class GridStatus:
    """The Tahoe grid's replication story."""

    shares_needed: int
    shares_happy: int
    shares_total: int
    storage_expected: int  # manifest nodes with the storage role
    storage_connected: (
        int | None
    )  # servers announced to the introducer and answering; None = unknown
    storage_identities: int | None = None  # announcements in total; > connected means churn
    storage_listening: int | None = (
        None  # storage nodes answering on their tub port; None = not probed
    )

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
    disk_total_bytes: int | None = None  # size of the storage disk, to verify contributions
    fetch_seconds: float | None = None  # how long the last census fetch took
    walk_seconds: float | None = None  # how long the node's last refresh took
    walk_mode: str | None = None  # "full" or "incremental"
    storage_listening: bool | None = None  # Tahoe answering on the tub port (None: not probed)


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
    quotas: list[MemberQuota] = field(default_factory=list)

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
        data["quotas"] = [
            {**asdict(quota), "percent": quota.percent, "over": quota.over} for quota in self.quotas
        ]
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
    if seconds < 48 * 3600:
        return f"{seconds / 3600:.1f}h"
    return f"{seconds / 86400:.1f}d"


def _collect_replication(
    nodes: list[NodeStatus],
    grid: GridStatus,
    fetch_census: CensusFetcher,
    notes: list[str],
    store: CensusStore,
    now: datetime | None = None,
    storage_down: frozenset[str] = frozenset(),
) -> ReplicationStatus | None:
    """Aggregate the storage nodes' share censuses into replication counts.

    Inventory precedence per node: live answer > the store's last answer
    (shares are immutable, so a stale inventory stays accurate) > assumed
    empty for a node that has never reported (a brand-new node genuinely
    holds nothing). This keeps the census computable through outages;
    availability counts (live copies only) carry the actual risk signal.
    """
    now = now or datetime.now(UTC)
    now_ts = now.timestamp()
    storage_nodes = [n for n in nodes if "tahoe_storage" in n.roles]
    if not storage_nodes:
        return None

    missing: list[str] = []
    ages: dict[str, float | None] = {}
    for node in storage_nodes:
        payload = fetch_census(node.vpn_ip)
        if payload:
            store.ingest(node.name, node.vpn_ip, payload, now_ts)
            # The census sidecar answering says nothing about Tahoe itself:
            # a node whose storage server is down keeps its inventory (the
            # shares are on its disk) but serves none of it right now.
            store.set_live(node.name, node.name not in storage_down)
            continue
        missing.append(node.name)
        age = store.mark_unreachable(node.name, now_ts)
        ages[node.name] = age
        if age is None:
            notes.append(f"{node.name} has never reported a census; assuming empty")
        else:
            notes.append(f"{node.name} unreachable; using its census from {_human_age(age)} ago")
    store.prune(n.name for n in storage_nodes)

    records = store.records()
    per_server: dict[str, ServerCensus] = {}
    for node in storage_nodes:
        record = records.get(node.name)
        if record is None:
            per_server[node.name] = ServerCensus(0, 0, source="assumed-empty")
            continue
        per_server[node.name] = ServerCensus(
            objects=record.object_count,
            disk_used_bytes=record.disk_used_bytes,
            source="live" if node.name not in missing else "cached",
            age_seconds=ages.get(node.name),
            disk_total_bytes=record.disk_total_bytes,
            fetch_seconds=record.fetch_seconds,
            walk_seconds=record.walk_seconds,
            walk_mode=record.walk_mode,
            storage_listening=node.storage_listening,
        )

    if (
        missing
        and all(census.source == "assumed-empty" for census in per_server.values())
        and store.rows() == 0
    ):
        # Nothing has ever reported: there is genuinely nothing to count yet.
        return None

    target = min(grid.shares_total, len(storage_nodes))
    summary = store.summary(target, grid.shares_needed)
    return ReplicationStatus(
        objects_total=summary.objects_total,
        target_copies=target,
        fully_replicated=summary.fully_replicated,
        under_replicated=summary.objects_total - summary.fully_replicated,
        complete=not missing,
        available_full=summary.available_full,
        unreadable_now=summary.unreadable_now,
        per_server=per_server,
    )


def _collect_usage(
    nodes: list[NodeStatus],
    fetch_usage: UsageFetcher,
    usage_cache_dir: Path | None = None,
    now: datetime | None = None,
) -> dict[str, dict[str, Any]]:
    """The client nodes' usage reports, keyed by node name.

    Same precedence as the census: a live answer, else the last cached one
    (marked ``source: cached``). A node that never reported is simply absent,
    so its member shows no usage rather than a wrong zero.
    """
    now = now or datetime.now(UTC)
    reports: dict[str, dict[str, Any]] = {}
    for node in nodes:
        # Any node with a Tahoe role may run a client (the manifest's roles are
        # not always complete); a node without the meter refuses instantly.
        if node.is_self or not any(r in node.roles for r in ("tahoe_client", "tahoe_storage")):
            continue
        payload = fetch_usage(node.vpn_ip)
        if payload:
            if usage_cache_dir is not None:
                save_census(usage_cache_dir, node.name, payload, now)
            reports[node.name] = {**payload, "source": "live"}
            continue
        cached = load_census(usage_cache_dir, node.name, now) if usage_cache_dir else None
        if cached is not None:
            reports[node.name] = {**cached[0], "source": "cached"}
    return reports


def collect_status(
    manifest: dict[str, Any],
    self_name: str,
    ping: Pinger,
    storage_connected: int | None,
    furl_present: bool,
    manifest_synced_at: datetime | None,
    now: datetime | None = None,
    fetch_census: CensusFetcher | None = None,
    census_store: CensusStore | None = None,
    fetch_usage: UsageFetcher | None = None,
    usage_cache_dir: Path | None = None,
    announcements: list[Announcement] | None = None,
    probe_storage: StorageProber | None = None,
) -> NetworkStatus:
    """Build the status model from the raw inputs.

    ``storage_connected`` is the introducer's announcement count; when the
    parsed ``announcements`` are given they take precedence, counting
    distinct servers (a node that recreated its volumes announces a new
    identity each time, and the introducer never forgets the old ones).
    With ``probe_storage`` every reachable storage node is also asked on
    its tub port, and the count of servers is the lower of the two: the
    introducer remembers a server that went down, the probe does not.
    """
    now = now or datetime.now(UTC)
    network = manifest.get("network", {}) or {}
    tahoe = network.get("tahoe", {}) or {}
    notes: list[str] = []
    storage_identities: int | None = None
    if announcements is not None:
        storage_names = [
            str(n.get("name"))
            for n in manifest.get("nodes", []) or []
            if "tahoe_storage" in (n.get("roles") or [])
        ]
        identities = summarize_storage(announcements, storage_names)
        storage_connected = identities.servers
        storage_identities = identities.identities
        notes.extend(identity_notes(identities))

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

    storage_listening: int | None = None
    storage_down: list[str] = []
    if probe_storage is not None:
        storage_listening = 0
        for node in nodes:
            if "tahoe_storage" not in node.roles or node.is_self or not node.reachable:
                continue
            node.storage_listening = probe_storage(node.vpn_ip)
            if node.storage_listening:
                storage_listening += 1
            elif node.storage_listening is False:
                storage_down.append(node.name)
        if storage_connected is None or storage_listening < storage_connected:
            storage_connected = storage_listening

    grid = GridStatus(
        shares_needed=int(tahoe.get("shares_needed", 3)),
        shares_happy=int(tahoe.get("shares_happy", 7)),
        shares_total=int(tahoe.get("shares_total", 10)),
        storage_expected=sum(1 for n in nodes if "tahoe_storage" in n.roles),
        storage_connected=storage_connected,
        storage_identities=storage_identities,
        storage_listening=storage_listening,
    )

    # --- overall verdict -------------------------------------------------
    overall = "ok"
    for name in storage_down:
        notes.append(
            f"{name}: storage server not answering on port {STORAGE_TUB_PORT} "
            "(the node is up and its census answers; Tahoe is down or restarting)"
        )
        overall = "degraded"

    replication: ReplicationStatus | None = None
    if fetch_census is not None:
        # Without a store of its own the census lives for this call only.
        replication = _collect_replication(
            nodes,
            grid,
            fetch_census,
            notes,
            census_store or CensusStore(),
            now=now,
            storage_down=frozenset(storage_down),
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

    # --- members: contribution, allocation, usage ---------------------------
    # Visibility, not verdict: a member over allocation is noted for everyone
    # to see, but it is not a network fault, so it never degrades the status.
    disk_totals = (
        {name: census.disk_total_bytes for name, census in replication.per_server.items()}
        if replication is not None
        else {}
    )
    usage = _collect_usage(nodes, fetch_usage, usage_cache_dir, now) if fetch_usage else {}
    for node_name, report in sorted(usage.items()):
        age = report_age(report, now)
        if age is not None and age > STALE_USAGE_AFTER:
            notes.append(
                f"{node_name}: usage report is {_human_age(age)} old (its meter has not "
                f"completed a measurement since {report.get('computed_at')})"
            )
        if report.get("partial"):
            notes.append(
                f"{node_name}: usage report is partial ({report.get('skipped_dirs', 0)} "
                "directories could not be listed in time); usage is a lower bound"
            )
    quotas = compute_quotas(manifest, usage, disk_totals, now=now)
    for quota in quotas:
        if quota.over and quota.used_bytes is not None:
            notes.append(
                f"member {quota.member} is over allocation: "
                f"{format_size(quota.used_bytes)} used of {format_size(quota.allocation_bytes)}"
            )
        if quota.overstated:
            notes.append(
                f"{', '.join(quota.overstated)}: storage disk smaller than the declared "
                "storage_contribution; the real size counts"
            )

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
        quotas=quotas,
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


# --- hourly rollups (7-day / 30-day uptime) ---------------------------------
#
# Raw minute samples are kept for a couple of days only (MAX_HISTORY_LINES).
# Every completed hour is folded into one line here, per node how many samples
# saw it up out of how many saw it at all, so month-long windows cost a few
# hundred small records to read instead of tens of megabytes of raw samples.

MAX_ROLLUP_LINES = 24 * 90  # 90 days of hourly rollups


def _hour_key(ts: datetime) -> str:
    return ts.astimezone(UTC).strftime("%Y-%m-%dT%H")


def rollup_hours(history_path: Path, rollup_path: Path, now: datetime | None = None) -> int:
    """Fold every completed hour present in the raw history but missing from
    the rollup file into one line each. Idempotent; returns the lines added.

    A line is ``{"hour": "YYYY-MM-DDTHH", "samples": N, "up": {node: [up, seen]}}``.
    A node counts only in samples where it existed, so a node that joined
    mid-hour is measured over its own minutes. The current hour waits until it
    is complete.
    """
    now = now or datetime.now(UTC)
    current = _hour_key(now)

    existing: set[str] = set()
    try:
        for line in rollup_path.read_text().splitlines():
            try:
                existing.add(str(json.loads(line)["hour"]))
            except (ValueError, KeyError, TypeError):
                continue
    except OSError:
        pass

    try:
        lines = history_path.read_text().splitlines()
    except OSError:
        return 0
    samples: dict[str, int] = {}
    buckets: dict[str, dict[str, list[int]]] = {}
    for line in lines:
        try:
            sample = json.loads(line)
            hour = _hour_key(datetime.fromisoformat(sample["ts"]))
        except (ValueError, KeyError, TypeError):
            continue
        if hour >= current or hour in existing:
            continue
        samples[hour] = samples.get(hour, 0) + 1
        bucket = buckets.setdefault(hour, {})
        for name, up in (sample.get("up") or {}).items():
            counts = bucket.setdefault(str(name), [0, 0])
            counts[0] += 1 if up else 0
            counts[1] += 1
    if not buckets:
        return 0

    rollup_path.parent.mkdir(parents=True, exist_ok=True)
    with rollup_path.open("a") as f:
        for hour in sorted(buckets):
            f.write(
                json.dumps({"hour": hour, "samples": samples[hour], "up": buckets[hour]}) + "\n"
            )

    try:
        kept = rollup_path.read_text().splitlines()
    except OSError:
        return len(buckets)
    if len(kept) > MAX_ROLLUP_LINES:
        rollup_path.write_text("\n".join(kept[-MAX_ROLLUP_LINES:]) + "\n")
    return len(buckets)


def uptime_windows(
    rollup_path: Path, windows: dict[str, timedelta], now: datetime | None = None
) -> dict[str, dict[str, float]]:
    """Per-window, per-node uptime percentage from the hourly rollups.

    ``windows`` maps a label to its length, e.g. ``{"7d": timedelta(days=7)}``.
    A node with no samples in a window is absent from that window's result.
    """
    now = now or datetime.now(UTC)
    totals: dict[str, dict[str, list[int]]] = {label: {} for label in windows}
    try:
        lines = rollup_path.read_text().splitlines()
    except OSError:
        return {label: {} for label in windows}
    for line in lines:
        try:
            record = json.loads(line)
            hour_start = datetime.strptime(record["hour"], "%Y-%m-%dT%H").replace(tzinfo=UTC)
        except (ValueError, KeyError, TypeError):
            continue
        for label, window in windows.items():
            if hour_start < now - window:
                continue
            for node, counts in (record.get("up") or {}).items():
                try:
                    up, seen = int(counts[0]), int(counts[1])
                except (TypeError, ValueError, IndexError):
                    continue
                acc = totals[label].setdefault(str(node), [0, 0])
                acc[0] += up
                acc[1] += seen
    return {
        label: {node: round(100.0 * up / seen, 1) for node, (up, seen) in nodes.items() if seen}
        for label, nodes in totals.items()
    }
