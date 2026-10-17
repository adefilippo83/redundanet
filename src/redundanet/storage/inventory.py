"""Walk a client's aliases and measure its footprint on the grid.

Shared by the rebalancer (which re-encodes files whose capability carries an
old k-of-n) and the usage meter (which sums what a member occupies). Both
only need directory listings and capability parsing: a CHK capability
literally contains the file's encoding and size,
``URI:CHK:<key>:<hash>:<k>:<n>:<size>``, so no share is ever downloaded.

Directories are listed through the node's own web API (``GET /uri/<cap>
?t=json``), one request per directory, a few milliseconds each, instead of
one ``tahoe ls`` process per directory, which costs over a second of
interpreter start-up before the listing even begins. Immutable directories
(``tahoe backup``'s snapshots) never change, so their listings can be kept
in a cache between walks: a measurement then lists only what is new.

Pure functions with injected listers and runners, so all of it is
unit-testable.
"""

from __future__ import annotations

import json
import urllib.parse
import urllib.request
from collections.abc import Callable, Iterable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from redundanet.core.quota import footprint_bytes

# run(args, timeout=...) -> CompletedProcess-like (returncode, stdout, stderr)
Runner = Callable[..., Any]
Logger = Callable[[str], None]
# list_dir(cap) -> (node type, payload), as the web API's ?t=json gives it;
# raises on failure (a timeout included).
Lister = Callable[[str], tuple[str, dict[str, Any]]]
# One child of a directory: (name, "dirnode" | "filenode", capability or "").
Child = tuple[str, str, str]

DEFAULT_NODE_URL = "http://127.0.0.1:3456/"


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
    return list(alias_caps(run))


def alias_caps(run: Runner) -> dict[str, str]:
    """Alias name -> root directory capability, from ``tahoe list-aliases``."""
    result = run(["list-aliases"], timeout=60)
    if result.returncode != 0:
        return {}
    caps: dict[str, str] = {}
    for line in result.stdout.splitlines():
        name, sep, cap = line.partition(":")
        if sep and cap.strip().startswith("URI:"):
            caps[name.strip()] = cap.strip()
    return caps


def is_immutable_dir(cap: str) -> bool:
    """Whether a directory capability is immutable (``tahoe backup`` snapshots)."""
    return cap.startswith(("URI:DIR2-CHK:", "URI:DIR2-LIT:"))


# One directory listing may take this long. A backup snapshot of a flat share
# with ~100k files is one directory: about 335 s and 46 MB of JSON on a Pi.
LISTING_TIMEOUT = 900


def read_node_url(path: Path) -> str:
    """The client node's web API address (``node.url`` in its directory)."""
    try:
        return path.read_text().strip() or DEFAULT_NODE_URL
    except OSError:
        return DEFAULT_NODE_URL


def web_lister(node_url: str, timeout: float = LISTING_TIMEOUT) -> Lister:
    """A lister that asks the node's web API for a directory by capability.

    ``node_url`` is the node's own ``node.url`` (its local web port); only
    http and https are accepted, so a mangled file can never turn the
    walker into a reader of local files or other schemes.
    """
    if urllib.parse.urlsplit(node_url).scheme not in ("http", "https"):
        raise ValueError(f"node URL must be http(s), not {node_url!r}")
    base = node_url.rstrip("/")

    def list_dir(cap: str) -> tuple[str, dict[str, Any]]:
        url = f"{base}/uri/{urllib.parse.quote(cap, safe='')}?t=json"
        with urllib.request.urlopen(url, timeout=timeout) as response:  # noqa: S310  # nosec B310
            node_type, payload = json.load(response)
        return str(node_type), payload

    return list_dir


class DirectoryCache:
    """What a walk may keep between runs: the children of immutable
    directories. ``get`` returns None for an unknown directory."""

    def get(self, cap: str) -> list[Child] | None:
        raise NotImplementedError

    def put(self, cap: str, children: list[Child]) -> None:
        raise NotImplementedError


def _children(listing: tuple[str, dict[str, Any]]) -> list[Child]:
    node_type, payload = listing
    if node_type != "dirnode":
        return []
    children: list[Child] = []
    for name, (child_type, child) in sorted((payload.get("children") or {}).items()):
        cap = str(child.get("rw_uri") or child.get("ro_uri") or "")
        children.append((str(name), str(child_type), cap))
    return children


def _list(list_dir: Lister, cap: str) -> list[Child] | Exception:
    """A listing, or the exception that prevented it: never raises, so a
    batch of parallel listings always completes."""
    try:
        return _children(list_dir(cap))
    except Exception as e:
        return e


def walk_tree(
    root_cap: str,
    list_dir: Lister,
    *,
    label: str = "",
    cache: DirectoryCache | None = None,
    log: Logger | None = None,
    skip_immutable: bool = False,
    skipped: list[str] | None = None,
    workers: int = 1,
) -> list[tuple[str, str]]:
    """All (path, file cap) pairs reachable from a directory capability.
    Directories are walked, not returned. ``label`` prefixes the paths in
    log lines and in ``skipped`` (an alias spec like ``backups:``).

    A directory capability is visited once: ``tahoe backup`` links a new
    ``Archives/<timestamp>`` entry on every run, and unchanged runs point at
    the same immutable directory, so without this a backup alias would be
    walked once per snapshot. Immutable directories come from ``cache``
    when it has them and go into it when listed. ``skip_immutable`` leaves
    them out entirely, for callers that must relink files (the rebalancer):
    a file inside an immutable snapshot cannot be relinked. A directory
    whose listing fails is skipped with its subtree, logged and appended to
    ``skipped``; the walk goes on. Up to ``workers`` listings are in flight
    at once; the cache is only touched from the calling thread.
    """
    files: list[tuple[str, str]] = []
    seen: set[str] = {root_cap}
    pending: list[tuple[str, str]] = [("", root_cap)]
    batch_size = max(workers, 1) * 4
    executor = ThreadPoolExecutor(max_workers=workers) if workers > 1 else None
    try:
        while pending:
            batch, pending = pending[:batch_size], pending[batch_size:]
            results: list[tuple[str, str, list[Child] | Exception, bool]] = []
            to_list: list[tuple[str, str]] = []
            for path, cap in batch:
                cached = cache.get(cap) if cache is not None and is_immutable_dir(cap) else None
                if cached is not None:
                    results.append((path, cap, cached, True))
                else:
                    to_list.append((path, cap))
            if executor is None:
                results.extend((path, cap, _list(list_dir, cap), False) for path, cap in to_list)
            else:
                futures = [
                    (path, cap, executor.submit(_list, list_dir, cap)) for path, cap in to_list
                ]
                results.extend((path, cap, future.result(), False) for path, cap, future in futures)
            for path, cap, listing, from_cache in results:
                spec = f"{label}{path}"
                if isinstance(listing, Exception):
                    if log:
                        log(f"cannot list {spec!r} (skipping its subtree): {listing!s:.120}")
                    if skipped is not None:
                        skipped.append(spec)
                    continue
                if cache is not None and not from_cache and is_immutable_dir(cap):
                    cache.put(cap, listing)
                for name, child_type, child_cap in listing:
                    child_path = f"{path}/{name}" if path else name
                    if child_type == "dirnode":
                        if not child_cap or child_cap in seen:
                            continue
                        if skip_immutable and is_immutable_dir(child_cap):
                            continue
                        seen.add(child_cap)
                        pending.append((child_path, child_cap))
                    elif child_type == "filenode" and child_cap:
                        files.append((child_path, child_cap))
    finally:
        if executor is not None:
            executor.shutdown()
    return files


def all_file_caps(
    aliases: dict[str, str],
    list_dir: Lister,
    *,
    cache: DirectoryCache | None = None,
    log: Logger | None = None,
    skipped: list[str] | None = None,
    workers: int = 1,
) -> list[str]:
    """Every file capability reachable from the given aliases (name ->
    root cap). Directories that could not be listed are appended to
    ``skipped`` as ``alias:path``."""
    caps: list[str] = []
    for alias, root_cap in aliases.items():
        caps.extend(
            cap
            for _path, cap in walk_tree(
                root_cap,
                list_dir,
                label=f"{alias}:",
                cache=cache,
                log=log,
                skipped=skipped,
                workers=workers,
            )
        )
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
