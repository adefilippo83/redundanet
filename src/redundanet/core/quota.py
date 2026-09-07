"""Contribution-based allocation (tokens) and per-member usage.

The model: a member contributes disk to the network (tokens are the gigabytes
of ``storage_contribution`` on their nodes) and may store, across the grid,

    allocation = contribution * k/n * (1 - reserve)

because under k-of-n erasure coding every stored byte occupies n/k bytes of
grid capacity. 500 GB at 1-of-2 or 2-of-4 gives 250 GB; at 3-of-10 it gives
150 GB. The reserve keeps room for what the grid holds beyond live files:
shares of deleted files until their leases expire, and the rebalancer's
transition between encodings.

Usage is what a member's clients report: the sum, over their files, of
size * n_i/k_i with each file's own encoding read from its capability, i.e.
the member's real footprint on the grid. Storage servers cannot attribute a
share to a member (Tahoe keeps shares anonymous by design), so usage is
client-measured and client-enforced, and the status page shows it to every
member. In a vetted community that visibility is the enforcement.

Everything here is pure: the manifest and the reports are passed in.
"""

from __future__ import annotations

import math
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

DEFAULT_RESERVE = 0.15
DEFAULT_ENFORCE = False
STORAGE_ROLE = "tahoe_storage"
CLIENT_ROLE = "tahoe_client"

_SIZE_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*([a-z]*)\s*$", re.IGNORECASE)
# Decimal units like disk labels and Tahoe's reserved_space; binary with "i".
_UNITS: dict[str, int] = {
    "": 1,
    "b": 1,
    "k": 10**3,
    "kb": 10**3,
    "m": 10**6,
    "mb": 10**6,
    "g": 10**9,
    "gb": 10**9,
    "t": 10**12,
    "tb": 10**12,
    "p": 10**15,
    "pb": 10**15,
    "kib": 2**10,
    "mib": 2**20,
    "gib": 2**30,
    "tib": 2**40,
    "pib": 2**50,
}


def parse_size(text: str | None) -> int | None:
    """Bytes for a human size ("500GB", "1TB", "1G", "250 GiB"), or None."""
    if not text:
        return None
    match = _SIZE_RE.match(str(text))
    if not match:
        return None
    factor = _UNITS.get(match.group(2).lower())
    if factor is None:
        return None
    return int(float(match.group(1)) * factor)


def format_size(n: int) -> str:
    """Decimal, disk-label style: 250.0 GB, 1.20 TB, 800 MB."""
    if n >= 10**12:
        return f"{n / 10**12:.2f} TB"
    if n >= 10**9:
        return f"{n / 10**9:.1f} GB"
    if n >= 10**6:
        return f"{n / 10**6:.0f} MB"
    return f"{n} B"


def expansion(needed: int, total: int) -> float:
    """Grid bytes per stored byte under k-of-n: n/k."""
    return total / max(needed, 1)


def allocation_bytes(contribution: int, needed: int, total: int, reserve: float) -> int:
    """What a contribution lets a member store: contribution / (n/k), minus the reserve."""
    return int(contribution * max(needed, 1) / max(total, 1) * (1.0 - reserve))


def footprint_bytes(size: int, needed: int, total: int) -> int:
    """Grid capacity one file of ``size`` bytes occupies at k-of-n."""
    return math.ceil(size * expansion(needed, total))


@dataclass
class QuotaSettings:
    """Network-wide knobs, from the manifest's ``network.quota`` section."""

    reserve: float = DEFAULT_RESERVE
    enforce: bool = DEFAULT_ENFORCE


def quota_settings(manifest: dict[str, Any]) -> QuotaSettings:
    network = manifest.get("network") or {}
    raw = network.get("quota") or {}
    try:
        reserve = float(raw.get("reserve", DEFAULT_RESERVE))
    except (TypeError, ValueError):
        reserve = DEFAULT_RESERVE
    reserve = min(max(reserve, 0.0), 0.9)
    return QuotaSettings(reserve=reserve, enforce=bool(raw.get("enforce", DEFAULT_ENFORCE)))


def encoding(manifest: dict[str, Any]) -> tuple[int, int]:
    """(k, n) from the manifest, with the schema defaults."""
    tahoe = (manifest.get("network") or {}).get("tahoe") or {}
    return int(tahoe.get("shares_needed", 3)), int(tahoe.get("shares_total", 10))


def resolve_encoding(
    manifest: dict[str, Any], environ: Mapping[str, str] | None = None
) -> tuple[int, int, int]:
    """(needed, happy, total) for a node: the manifest first, the environment second.

    The manifest is the network's source of truth and every node syncs it, so
    a change there reaches the whole fleet at the next container start with
    no per-node reconfiguration. The ``REDUNDANET_SHARES_*`` variables (from
    the node's ``.env``) remain the fallback for a node without a manifest,
    e.g. the e2e tests.
    """
    environ = environ or {}
    tahoe = (manifest.get("network") or {}).get("tahoe") or {}

    def pick(key: str, env_key: str, default: int) -> int:
        if key in tahoe:
            try:
                return int(tahoe[key])
            except (TypeError, ValueError):
                pass
        try:
            return int(environ.get(env_key, "") or default)
        except ValueError:
            return default

    return (
        pick("shares_needed", "REDUNDANET_SHARES_NEEDED", 3),
        pick("shares_happy", "REDUNDANET_SHARES_HAPPY", 7),
        pick("shares_total", "REDUNDANET_SHARES_TOTAL", 10),
    )


def member_of(node: dict[str, Any]) -> str:
    """The member a node belongs to: its ``member`` field, else the node itself."""
    return str(node.get("member") or node.get("name") or "?")


def _roles(node: dict[str, Any]) -> list[str]:
    return [str(r) for r in node.get("roles") or []]


@dataclass
class MemberQuota:
    """One member's contribution, allocation and usage, for display and checks."""

    member: str
    nodes: list[str]
    contributed_bytes: int  # claimed: sum of storage_contribution over the member's storage nodes
    effective_bytes: int  # after capacity verification (a smaller disk counts as what it is)
    allocation_bytes: int
    used_bytes: int | None  # None: no client of this member has reported usage
    files: int = 0
    usage_source: str = "none"  # "live" | "cached" | "none"
    overstated: list[str] = field(default_factory=list)  # nodes whose disk is smaller than claimed

    @property
    def percent(self) -> float | None:
        if self.used_bytes is None or self.allocation_bytes <= 0:
            return None
        return round(100.0 * self.used_bytes / self.allocation_bytes, 1)

    @property
    def over(self) -> bool:
        return self.used_bytes is not None and self.used_bytes > self.allocation_bytes


def compute_quotas(
    manifest: dict[str, Any],
    usage: dict[str, dict[str, Any]] | None = None,
    disk_totals: dict[str, int | None] | None = None,
    reserve: float | None = None,
) -> list[MemberQuota]:
    """Per-member quotas from the manifest plus the clients' usage reports.

    ``usage`` maps a node name to its usage payload (``used_bytes``, ``files``,
    and a ``source`` of "live" or "cached"); ``disk_totals`` maps a storage
    node to the size of its storage disk as reported by its census (None when
    unknown), used to verify the claimed contribution.
    """
    usage = usage or {}
    disk_totals = disk_totals or {}
    settings = quota_settings(manifest)
    reserve = settings.reserve if reserve is None else reserve
    needed, total = encoding(manifest)

    groups: dict[str, list[dict[str, Any]]] = {}
    for node in manifest.get("nodes") or []:
        groups.setdefault(member_of(node), []).append(node)

    quotas: list[MemberQuota] = []
    for member, nodes in groups.items():
        contributed = 0
        effective = 0
        overstated: list[str] = []
        for node in nodes:
            if STORAGE_ROLE not in _roles(node):
                continue
            claimed = parse_size(node.get("storage_contribution")) or 0
            contributed += claimed
            actual = disk_totals.get(str(node.get("name")))
            if actual is not None and actual < claimed:
                overstated.append(str(node.get("name")))
                effective += actual
            else:
                effective += claimed

        used: int | None = None
        files = 0
        source = "none"
        for node in nodes:
            report = usage.get(str(node.get("name")))
            if not report:
                continue
            used = (used or 0) + int(report.get("used_bytes", 0))
            files += int(report.get("files", 0))
            report_source = str(report.get("source", "live"))
            if source == "none" or (report_source == "cached" and source == "live"):
                source = report_source if source == "none" else "cached"

        quotas.append(
            MemberQuota(
                member=member,
                nodes=[str(n.get("name")) for n in nodes],
                contributed_bytes=contributed,
                effective_bytes=effective,
                allocation_bytes=allocation_bytes(effective, needed, total, reserve),
                used_bytes=used,
                files=files,
                usage_source=source,
                overstated=overstated,
            )
        )
    return sorted(quotas, key=lambda q: q.member)


def node_allocation(manifest: dict[str, Any], node_name: str) -> tuple[str, int, QuotaSettings]:
    """(member, allocation bytes, settings) for the member a node belongs to.

    Used by the client side (usage meter, enforcement points), which knows only
    its own node name. An unknown node is its own member with no contribution.
    """
    settings = quota_settings(manifest)
    nodes = manifest.get("nodes") or []
    me = next((n for n in nodes if str(n.get("name")) == node_name), {"name": node_name})
    member = member_of(me)
    for quota in compute_quotas(manifest, reserve=settings.reserve):
        if quota.member == member:
            return member, quota.allocation_bytes, settings
    return member, 0, settings
