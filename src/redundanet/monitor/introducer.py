"""What the introducer knows about storage servers: announcements per identity.

A storage node announces itself under its server id, the signing key in the
Tahoe node's private directory. Recreate the node's volume and it comes back
as a new server: the introducer keeps every announcement it ever received
(it never expires them), so the old identity stays listed, clients keep
counting it, and the "storage servers" tile reads 9 of 7. The JSON view of
the introducer only carries totals; its status page lists each announcement
with nickname, server id and time, and that page is stable (Tahoe-LAFS 1.20,
dormant upstream). This module reads it and turns it into per-node facts:
how many servers are really announced, which node has more than one
identity (its volumes were recreated), and which announced nickname belongs
to no manifest node.

Pure: takes the page text and the manifest's storage node names.
"""

from __future__ import annotations

import html
import re
from dataclasses import dataclass, field

STORAGE_SUFFIX = "-storage"  # the entrypoint names a storage node "<node>-storage"

_ROW = re.compile(r"<tr[^>]*>(.*?)</tr>", re.S)
_NICK = re.compile(r'class="nickname">(.*?)</div>', re.S)
_NODEID = re.compile(r'class="nodeid data-chars">(.*?)</div>', re.S)
_ANNOUNCED = re.compile(r'class="service-announced"[^>]*>(.*?)</td>', re.S)
_SERVICE = re.compile(r'class="service-service-name">(.*?)</td>', re.S)


@dataclass(frozen=True)
class Announcement:
    nickname: str
    server_id: str  # "v0-<key>"
    announced_at: str  # as printed by the introducer, UTC, "YYYY-MM-DD HH:MM:SS"
    service: str

    @property
    def node_name(self) -> str:
        """The manifest node behind a storage nickname."""
        if self.nickname.endswith(STORAGE_SUFFIX):
            return self.nickname[: -len(STORAGE_SUFFIX)]
        return self.nickname


def parse_announcements(page: str) -> list[Announcement]:
    """Service announcements from the introducer's status page. Subscriber
    rows (clients) carry a different time column and are left out."""
    found: list[Announcement] = []
    for row in _ROW.findall(page):
        announced = _ANNOUNCED.search(row)
        nick = _NICK.search(row)
        node_id = _NODEID.search(row)
        service = _SERVICE.search(row)
        if not (announced and nick and node_id and service):
            continue
        found.append(
            Announcement(
                nickname=_text(nick.group(1)),
                server_id=_text(node_id.group(1)),
                announced_at=_text(announced.group(1)),
                service=_text(service.group(1)),
            )
        )
    return found


def _text(fragment: str) -> str:
    return " ".join(html.unescape(re.sub(r"<[^>]+>", " ", fragment)).split())


@dataclass
class StorageIdentities:
    """Storage announcements folded per node."""

    servers: int  # distinct nicknames announcing the storage service
    identities: int  # announcements in total (what the introducer's JSON counts)
    per_node: dict[str, list[Announcement]] = field(default_factory=dict)
    unknown: list[str] = field(default_factory=list)  # nicknames of no manifest node

    @property
    def churned(self) -> dict[str, list[Announcement]]:
        """Nodes announced under more than one identity, oldest first."""
        return {
            node: sorted(items, key=lambda a: a.announced_at)
            for node, items in self.per_node.items()
            if len(items) > 1
        }


def summarize_storage(
    announcements: list[Announcement], storage_nodes: list[str]
) -> StorageIdentities:
    storage = [a for a in announcements if a.service == "storage"]
    per_node: dict[str, list[Announcement]] = {}
    for item in storage:
        per_node.setdefault(item.node_name, []).append(item)
    known = set(storage_nodes)
    return StorageIdentities(
        servers=len({a.nickname for a in storage}),
        identities=len(storage),
        per_node=per_node,
        unknown=sorted(node for node in per_node if node not in known),
    )


def identity_notes(summary: StorageIdentities) -> list[str]:
    notes: list[str] = []
    for node, items in summary.churned.items():
        first, last = items[0].announced_at[:16], items[-1].announced_at[:16]
        notes.append(
            f"{node} has announced {len(items)} storage identities since {first} UTC "
            f"(last {last}): its volumes were recreated; the earlier identities are "
            "stale until the introducer restarts"
        )
    for node in summary.unknown:
        notes.append(f"storage server {node!r} is announced but is not in the manifest")
    return notes
