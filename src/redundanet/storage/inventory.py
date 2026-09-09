"""Walk a client's aliases and measure its footprint on the grid.

Shared by the rebalancer (which re-encodes files whose capability carries an
old k-of-n) and the usage meter (which sums what a member occupies). Both
only need ``tahoe ls --json`` walks and capability parsing: a CHK capability
literally contains the file's encoding and size,
``URI:CHK:<key>:<hash>:<k>:<n>:<size>``, so no share is ever downloaded.

Pure functions with an injected ``run`` (the tahoe CLI runner), so all of it
is unit-testable.
"""

from __future__ import annotations

import json
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from typing import Any

from redundanet.core.quota import footprint_bytes

# run(args, timeout=...) -> CompletedProcess-like (returncode, stdout, stderr)
Runner = Callable[..., Any]
Logger = Callable[[str], None]


@dataclass(frozen=True)
class ChkParams:
    needed: int
    total: int
    size: int


def parse_chk(cap: str) -> ChkParams | None:
    """Encoding and size of an immutable CHK capability, or None for anything else.

    LIT caps (tiny files inlined in the cap) have no shares; directories and
    mutables are small and out of scope.
    """
    parts = cap.strip().split(":")
    if len(parts) < 7 or parts[0] != "URI" or parts[1] != "CHK":
        return None
    try:
        return ChkParams(int(parts[4]), int(parts[5]), int(parts[6]))
    except ValueError:
        return None


def parse_chk_params(cap: str) -> tuple[int, int] | None:
    """(k, n) of a CHK capability, or None (the rebalancer's view)."""
    params = parse_chk(cap)
    return None if params is None else (params.needed, params.total)


def parse_encoding(cap: str) -> tuple[int, int] | None:
    """(k, n) of any immutable capability that carries shares: files
    (``URI:CHK:``) and ``tahoe backup``'s directories (``URI:DIR2-CHK:``).

    None for everything else: LIT caps inline their content and have no
    shares; mutable directories are re-encoded by their owner on every write.
    """
    parts = cap.strip().split(":")
    if len(parts) < 7 or parts[0] != "URI" or parts[1] not in ("CHK", "DIR2-CHK"):
        return None
    try:
        return int(parts[4]), int(parts[5])
    except ValueError:
        return None


def list_aliases(run: Runner) -> list[str]:
    result = run(["list-aliases"], timeout=60)
    if result.returncode != 0:
        return []
    return [line.split(":", 1)[0].strip() for line in result.stdout.splitlines() if ":" in line]


def is_immutable_dir(cap: str) -> bool:
    """Whether a directory capability is immutable (``tahoe backup`` snapshots)."""
    return cap.startswith(("URI:DIR2-CHK:", "URI:DIR2-LIT:"))


def walk_files(
    root: str,
    run: Runner,
    log: Logger | None = None,
    *,
    skip_immutable: bool = False,
) -> list[tuple[str, str]]:
    """All (grid path, file cap) pairs reachable from ``root`` (an alias spec
    like ``backups:``), via recursive ``tahoe ls --json``. Directories are
    walked, not returned. A listing that fails is skipped, not fatal.

    A directory capability is visited once: ``tahoe backup`` links a new
    ``Archives/<timestamp>`` entry on every run, and unchanged runs point at
    the same immutable directory, so without this a backup alias would be
    walked once per snapshot. ``skip_immutable`` leaves immutable directories
    out entirely, for callers that must relink files (the rebalancer): a file
    inside an immutable snapshot cannot be relinked.
    """
    files: list[tuple[str, str]] = []
    seen_dirs: set[str] = set()
    pending: list[str] = [""]
    while pending:
        subpath = pending.pop()
        spec = f"{root}{subpath}"
        result = run(["ls", "--json", spec], timeout=300)
        if result.returncode != 0:
            if log:
                log(f"cannot list {spec!r} (skipping): {result.stderr.strip()[:120]}")
            continue
        try:
            node_type, payload = json.loads(result.stdout)
        except (ValueError, TypeError):
            if log:
                log(f"unparseable listing for {spec!r} (skipping)")
            continue
        if node_type != "dirnode":
            continue
        for name, (child_type, child) in sorted((payload.get("children") or {}).items()):
            child_path = f"{subpath}/{name}" if subpath else name
            cap = child.get("rw_uri") or child.get("ro_uri") or ""
            if child_type == "dirnode":
                if cap and cap in seen_dirs:
                    continue
                if skip_immutable and is_immutable_dir(cap):
                    continue
                if cap:
                    seen_dirs.add(cap)
                pending.append(child_path)
            elif child_type == "filenode" and cap:
                files.append((child_path, cap))
    return files


def all_file_caps(run: Runner, log: Logger | None = None) -> list[str]:
    """Every file capability reachable from every alias of this client."""
    caps: list[str] = []
    for alias in list_aliases(run):
        caps.extend(cap for _path, cap in walk_files(f"{alias}:", run, log=log))
    return caps


@dataclass(frozen=True)
class Footprint:
    used_bytes: int  # grid capacity occupied: sum of size * n/k, per file's own encoding
    data_bytes: int  # plain file bytes
    files: int  # CHK files counted (LIT and directory objects are negligible)


def grid_footprint(caps: Iterable[str]) -> Footprint:
    """Sum the grid capacity the given file capabilities occupy.

    Each capability counts once, however many paths link it: the grid stores
    one copy (convergent encryption within a client), and backup snapshots
    link the same unchanged files over and over.
    """
    used = data = files = 0
    seen: set[str] = set()
    for cap in caps:
        if cap in seen:
            continue
        seen.add(cap)
        params = parse_chk(cap)
        if params is None:
            continue
        files += 1
        data += params.size
        used += footprint_bytes(params.size, params.needed, params.total)
    return Footprint(used_bytes=used, data_bytes=data, files=files)
