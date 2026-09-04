"""Multiple Tahoe introducers.

Tahoe reads its primary introducer from ``tahoe.cfg`` (``[client]
introducer.furl``, which Tahoe files under the reserved petname ``default``)
and any number of additional introducers from ``private/introducers.yaml``.
A storage node announces itself to every introducer it knows and a client
learns servers from every one of them, so a grid with two introducers keeps
working when either is down.

The manifest carries the FURLs: the historical top-level ``introducer_furl``
(the primary) plus, on each node with the ``tahoe_introducer`` role, the FURL
that node publishes as its own ``introducer_furl``.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml

from redundanet.core.exceptions import StorageError
from redundanet.storage.furl import parse_furl

INTRODUCERS_FILE = "introducers.yaml"
INTRODUCER_ROLE = "tahoe_introducer"


def dedupe(furls: list[str]) -> list[str]:
    """Strip, drop empties and duplicates, keep first-seen order."""
    seen: set[str] = set()
    out: list[str] = []
    for furl in furls:
        cleaned = furl.strip()
        if cleaned and cleaned not in seen:
            seen.add(cleaned)
            out.append(cleaned)
    return out


def introducer_furls_from_manifest(manifest: dict[str, Any]) -> list[str]:
    """All introducer FURLs a manifest declares: primary first, de-duplicated.

    The top-level ``introducer_furl`` comes first, then each introducer-role
    node's own ``introducer_furl`` in manifest order. A FURL on a node without
    the ``tahoe_introducer`` role is ignored.
    """
    furls: list[str] = []
    top = manifest.get("introducer_furl")
    if top:
        furls.append(str(top))
    for node in manifest.get("nodes") or []:
        if not isinstance(node, dict):
            continue
        furl = node.get("introducer_furl")
        if furl and INTRODUCER_ROLE in (node.get("roles") or []):
            furls.append(str(furl))
    return dedupe(furls)


def petname(furl: str, index: int) -> str:
    """A stable, filesystem-safe petname for an extra introducer.

    Tahoe names the per-introducer cache file after the petname, so it must be
    a plain identifier, and it must never be ``default`` (reserved for the
    tahoe.cfg entry). The tub id makes it stable across manifest reorderings.
    """
    try:
        tubid = parse_furl(furl)["tubid"]
    except StorageError:
        return f"intro{index}"
    return f"intro-{tubid[:12]}"


def render_introducers_yaml(extra_furls: list[str]) -> str:
    """The ``private/introducers.yaml`` body Tahoe expects."""
    entries = {
        petname(furl, index): {"furl": furl} for index, furl in enumerate(dedupe(extra_furls), 1)
    }
    return str(yaml.safe_dump({"introducers": entries}, sort_keys=True))


def write_introducers_yaml(private_dir: Path, extra_furls: list[str]) -> Path | None:
    """Write ``private/introducers.yaml`` for the extra introducers, or remove it.

    Returns the file path when written. With no extras the file is removed so a
    retired introducer does not linger in the node's configuration.
    """
    path = private_dir / INTRODUCERS_FILE
    extras = dedupe(extra_furls)
    if not extras:
        if path.exists():
            path.unlink()
        return None
    private_dir.mkdir(parents=True, exist_ok=True)
    path.write_text(render_introducers_yaml(extras))
    path.chmod(0o600)
    return path
