"""Usage reports: what a client node's member occupies on the grid.

The usage meter sidecar in the tahoe-client container computes the payload
periodically, serves it at ``GET /usage`` on the node's VPN address (the hub
aggregates it per member for the status page) and writes it to
``USAGE_FILE`` inside the client volume, where the local enforcement points
(the backup sync, ``redundanet storage upload``) read it.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from redundanet.core.quota import encoding, node_allocation
from redundanet.storage.inventory import Footprint

USAGE_PORT = 3460  # served on the node's VPN IP only
USAGE_FILE = Path("/var/lib/tahoe-client/redundanet-usage.json")


def usage_payload(
    node_name: str,
    manifest: dict[str, Any],
    footprint: Footprint,
    now: datetime | None = None,
    enforce_override: bool | None = None,
    in_progress: Footprint | None = None,
) -> dict[str, Any]:
    """The JSON body served at /usage and written to USAGE_FILE.

    ``footprint`` is everything the member occupies, including uploads of a
    backup still running (recorded in the backupdb, not yet linked into a
    snapshot); ``in_progress`` is that unlinked part on its own, so the page
    can say "N files uploading" instead of showing nothing for a day.
    """
    now = now or datetime.now(UTC)
    member, allocation, settings = node_allocation(manifest, node_name)
    needed, total = encoding(manifest)
    enforce = settings.enforce if enforce_override is None else enforce_override
    pending = in_progress or Footprint(0, 0, 0)
    return {
        "node": node_name,
        "member": member,
        "used_bytes": footprint.used_bytes,
        "data_bytes": footprint.data_bytes,
        "files": footprint.files,
        "in_progress_files": pending.files,
        "in_progress_bytes": pending.used_bytes,
        "allocation_bytes": allocation,
        "reserve": settings.reserve,
        "enforce": enforce,
        "encoding": f"{needed}-of-{total}",
        "computed_at": now.isoformat(timespec="seconds"),
    }


def load_usage_file(path: Path = USAGE_FILE) -> dict[str, Any] | None:
    """The last usage payload the meter wrote, or None when absent/unreadable."""
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return None
    return data if isinstance(data, dict) else None


def write_usage_file(payload: dict[str, Any], path: Path = USAGE_FILE) -> None:
    """Atomic write so a reader never sees a half-written file."""
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(payload))
    tmp.replace(path)


def over_allocation(payload: dict[str, Any], extra_bytes: int = 0) -> bool:
    """Whether storing ``extra_bytes`` more (grid footprint) would exceed the
    allocation, when the network enforces quotas. Unknown data never blocks."""
    if not payload.get("enforce"):
        return False
    try:
        used = int(payload.get("used_bytes", 0))
        allocation = int(payload.get("allocation_bytes", 0))
    except (TypeError, ValueError):
        return False
    return used + extra_bytes > allocation
