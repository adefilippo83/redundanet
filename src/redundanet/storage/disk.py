"""The storage disk's identity: a marker the node writes on its shares disk
and remembers, so a start on any other filesystem is refused.

On most nodes the shares directory is a bind mount of an external disk. When
that disk is not mounted by the time Docker starts (a USB drive or an iSCSI
target that comes up after the boot), the bind captures the empty directory
under the mountpoint and the storage server runs on the system disk: every
share the node holds looks gone to the network, every new one lands on the
wrong disk, and nothing says so until the hub reports the node smaller than
declared. Two days of that happened in production (October 2026).

So the entrypoint writes a random token to a marker file in the shares
directory on the node's first start and keeps a copy, the record, in the
node directory (a Docker volume, hence always mounted). Every later start
reads both: a missing or different marker means a different filesystem and
the node refuses to run. The census sidecar makes the same check before each
walk, so the hub is never handed an inventory of the wrong disk.
"""

from __future__ import annotations

import secrets
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

MARKER_FILE = ".redundanet-disk-id"  # in the shares directory, i.e. on the disk
RECORD_FILE = "redundanet-disk-id"  # in the node directory, i.e. in the volume

Status = Literal["ok", "first", "mismatch", "error"]


@dataclass(frozen=True)
class DiskCheck:
    """The outcome of comparing the marker on the disk with the record.

    ``first``: no record yet, the node has never marked a disk (establish one
    once the node directory exists). ``ok``: same token. ``mismatch``: the
    marker is missing or holds another token. ``error``: a file could not be
    read, which for the shares disk means it is not usable either.
    """

    status: Status
    expected: str | None = None  # the record's token
    found: str | None = None  # the marker's token
    error: str = ""

    @property
    def refused(self) -> bool:
        return self.status in ("mismatch", "error")


def read_token(path: Path) -> str | None:
    """The token in ``path``; None when the file is absent or empty."""
    try:
        text = path.read_text()
    except FileNotFoundError:
        return None
    return text.strip() or None


def verify(marker: Path, record: Path) -> DiskCheck:
    try:
        expected = read_token(record)
    except OSError as e:
        return DiskCheck("error", error=f"cannot read {record}: {e}")
    if expected is None:
        return DiskCheck("first")
    try:
        found = read_token(marker)
    except OSError as e:
        return DiskCheck("error", expected=expected, error=f"cannot read {marker}: {e}")
    if found == expected:
        return DiskCheck("ok", expected=expected, found=found)
    return DiskCheck("mismatch", expected=expected, found=found)


def establish(marker: Path, record: Path) -> str:
    """Make the disk under ``marker`` this node's: a marker already there is
    adopted (the node directory was recreated around an intact disk, or the
    disk came from another node), otherwise a fresh token is written; the
    record gets the copy. Returns the token."""
    token = read_token(marker)
    if token is None:
        token = secrets.token_hex(16)
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text(token + "\n")
    record.parent.mkdir(parents=True, exist_ok=True)
    record.write_text(token + "\n")
    return token


def explain(check: DiskCheck, marker: Path, record: Path) -> str:
    """What is wrong and what to do about it, for the container log: the
    operator reads this with no other clue."""
    if check.status == "error":
        what = f"the disk cannot be read ({check.error})"
    elif check.found is None:
        what = f"the marker {marker} is missing"
    else:
        what = f"the marker {marker} holds token {check.found}"
    expects = f", the record {record} expects {check.expected}" if check.expected else ""
    return (
        f"The shares directory {marker.parent} is not the disk this node was started on: "
        f"{what}{expects}. Usually the storage disk is not mounted on the host and Docker "
        "bound the empty directory under the mountpoint instead: check findmnt for the "
        "disk, mount it, then restart the storage services (redundanet storage stop && "
        "redundanet storage start; a running container does not see a mount made after "
        "its start). If the disk was replaced on purpose, copy the marker file to the "
        f"new disk, or delete the record (docker exec redundanet-tahoe-storage rm {record}) "
        "and restart: the next start adopts the disk it finds."
    )
