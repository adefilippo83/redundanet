"""Lease renewal and repair sweep: everything a client keeps alive on the grid.

Storage nodes garbage-collect shares whose lease has not been renewed within
the lease duration (90 days by default), so every object a member still
wants must get a fresh lease well inside that window: the files and the
directories reachable from the client's aliases, and the files a backup
still in progress has uploaded but not linked into a snapshot yet (the
backup database is their only record). The sweep checks each object once
through the node's web API with ``add-lease=true`` (and ``repair=true``
unless disabled), several at a time, and records every result, so a sweep
interrupted by a restart or by its time budget resumes where it stopped
instead of starting over. A ``deep-check`` of the same tree checks objects
one by one, counts shared snapshot directories once per snapshot, and
cannot resume: on a hundred thousand objects it never finished.

Pure functions with injected listers, checkers and clocks.
"""

from __future__ import annotations

import json
import sqlite3
import time
import urllib.parse
import urllib.request
from collections.abc import Callable, Iterable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from redundanet.storage.inventory import DirectoryCache, Lister, Logger, walk_tree

# Capabilities that carry no shares, so nothing to renew or repair.
NO_SHARES = ("URI:LIT:", "URI:DIR2-LIT:")
# A repair downloads and re-uploads a file; give a large one time.
CHECK_TIMEOUT = 1800

# check(cap) -> the web API's JSON for t=check; raises on failure.
Checker = Callable[[str], dict[str, Any]]


@dataclass(frozen=True)
class Target:
    cap: str
    kind: str  # "file" | "dir" | "pending" (uploaded by a backup, not linked yet)


def has_shares(cap: str) -> bool:
    return not cap.startswith(NO_SHARES)


def collect_targets(
    aliases: dict[str, str],
    list_dir: Lister,
    *,
    pending_caps: Iterable[str] = (),
    cache: DirectoryCache | None = None,
    log: Logger | None = None,
    skipped: list[str] | None = None,
    workers: int = 1,
) -> list[Target]:
    """Every object to renew, each once: directories first (the alias roots
    included), then files, then the pending uploads."""
    directories: list[tuple[str, str]] = []
    files: list[tuple[str, str]] = []
    for alias, root_cap in aliases.items():
        files.extend(
            walk_tree(
                root_cap,
                list_dir,
                label=f"{alias}:",
                cache=cache,
                log=log,
                skipped=skipped,
                workers=workers,
                directories=directories,
            )
        )
    seen: set[str] = set()
    targets: list[Target] = []
    for kind, caps in (
        ("dir", (cap for _path, cap in directories)),
        ("file", (cap for _path, cap in files)),
        ("pending", pending_caps),
    ):
        for cap in caps:
            if cap in seen or not has_shares(cap):
                continue
            seen.add(cap)
            targets.append(Target(cap, kind))
    return targets


# --- checking ------------------------------------------------------------


@dataclass(frozen=True)
class Outcome:
    healthy: bool
    repair_attempted: bool = False
    repair_successful: bool = False
    shares_good: int | None = None


def parse_check(data: dict[str, Any]) -> Outcome:
    """The web API's JSON for ``t=check``, with or without ``repair=true``."""
    if "repair-attempted" in data:
        post = (data.get("post-repair-results") or {}).get("results") or {}
        pre = (data.get("pre-repair-results") or {}).get("results") or {}
        results = post or pre
        return Outcome(
            healthy=bool(results.get("healthy", False)),
            repair_attempted=bool(data.get("repair-attempted")),
            repair_successful=bool(data.get("repair-successful")),
            shares_good=results.get("count-shares-good"),
        )
    results = data.get("results") or {}
    return Outcome(
        healthy=bool(results.get("healthy", False)),
        shares_good=results.get("count-shares-good"),
    )


def web_checker(node_url: str, repair: bool = True, timeout: float = CHECK_TIMEOUT) -> Checker:
    """A checker that asks the node's web API to check an object, renew its
    leases and repair it when shares are missing."""
    if urllib.parse.urlsplit(node_url).scheme not in ("http", "https"):
        raise ValueError(f"node URL must be http(s), not {node_url!r}")
    base = node_url.rstrip("/")
    query = "t=check&add-lease=true&output=JSON" + ("&repair=true" if repair else "")

    def check(cap: str) -> dict[str, Any]:
        url = f"{base}/uri/{urllib.parse.quote(cap, safe='')}?{query}"
        request = urllib.request.Request(url, data=b"", method="POST")  # noqa: S310  # nosec B310
        with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310  # nosec B310
            data = json.load(response)
        if not isinstance(data, dict):
            raise ValueError("unexpected check response")
        return data

    return check


# --- progress ------------------------------------------------------------

SCHEMA = """
CREATE TABLE IF NOT EXISTS sweeps (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at REAL NOT NULL,
    finished_at REAL,
    checked INTEGER NOT NULL DEFAULT 0,
    unhealthy INTEGER NOT NULL DEFAULT 0,
    repaired INTEGER NOT NULL DEFAULT 0,
    failed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS objects (
    cap TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    checked_at REAL NOT NULL,
    healthy INTEGER NOT NULL,
    repaired INTEGER NOT NULL
) WITHOUT ROWID;
"""


class SweepStore:
    """What the sweep did, on the client's volume: which objects the current
    sweep has already checked (so a restart resumes), and when the last
    sweep finished (so the next one is due at the right time)."""

    def __init__(self, path: Path | str = ":memory:") -> None:
        self._conn = sqlite3.connect(str(path), timeout=30)
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA synchronous=NORMAL")
        self._conn.executescript(SCHEMA)

    def close(self) -> None:
        self._conn.close()

    def current(self) -> tuple[int, float] | None:
        """(id, started_at) of the unfinished sweep, if any."""
        row = self._conn.execute(
            "SELECT id, started_at FROM sweeps WHERE finished_at IS NULL ORDER BY id DESC LIMIT 1"
        ).fetchone()
        return None if row is None else (int(row[0]), float(row[1]))

    def last_finished_at(self) -> float | None:
        row = self._conn.execute("SELECT max(finished_at) FROM sweeps").fetchone()
        return None if row is None or row[0] is None else float(row[0])

    def start(self, now: float) -> tuple[int, float]:
        with self._conn:
            cursor = self._conn.execute("INSERT INTO sweeps (started_at) VALUES (?)", (now,))
        return int(cursor.lastrowid or 0), now

    def finish(self, sweep_id: int, now: float, stats: SweepStats) -> None:
        with self._conn:
            self._conn.execute(
                "UPDATE sweeps SET finished_at = ?, checked = ?, unhealthy = ?, repaired = ?,"
                " failed = ? WHERE id = ?",
                (now, stats.checked, stats.unhealthy, stats.repaired, stats.failed, sweep_id),
            )

    def done_since(self, started_at: float) -> set[str]:
        """Objects checked since the sweep started: skipped on resume."""
        rows = self._conn.execute(
            "SELECT cap FROM objects WHERE checked_at >= ?", (started_at,)
        ).fetchall()
        return {str(row[0]) for row in rows}

    def record(self, target: Target, outcome: Outcome, now: float) -> None:
        with self._conn:
            self._conn.execute(
                "INSERT INTO objects (cap, kind, checked_at, healthy, repaired) VALUES (?, ?, ?, ?, ?)"
                " ON CONFLICT(cap) DO UPDATE SET kind = excluded.kind,"
                " checked_at = excluded.checked_at, healthy = excluded.healthy,"
                " repaired = excluded.repaired",
                (
                    target.cap,
                    target.kind,
                    now,
                    int(outcome.healthy),
                    int(outcome.repair_attempted and outcome.repair_successful),
                ),
            )

    def forget(self, before: float) -> int:
        """Drop records older than ``before`` (objects that are gone)."""
        with self._conn:
            cursor = self._conn.execute("DELETE FROM objects WHERE checked_at < ?", (before,))
        return int(cursor.rowcount)


# --- the sweep -----------------------------------------------------------


@dataclass
class SweepStats:
    targets: int = 0
    already_done: int = 0
    checked: int = 0
    healthy: int = 0
    unhealthy: int = 0
    repaired: int = 0
    failed: int = 0
    budget_stop: bool = False
    seconds: float = 0.0
    unhealthy_caps: list[str] = field(default_factory=list)

    @property
    def complete(self) -> bool:
        return not self.budget_stop and self.failed == 0

    def summary(self) -> str:
        return (
            f"{self.checked} objects checked ({self.already_done} already done), "
            f"{self.healthy} healthy, {self.unhealthy} unhealthy, {self.repaired} repaired, "
            f"{self.failed} failed, {self.seconds:.0f}s"
            + (" (time budget reached, resuming later)" if self.budget_stop else "")
        )


def _check(check: Checker, target: Target) -> Outcome | Exception:
    try:
        return parse_check(check(target.cap))
    except Exception as e:
        return e


def run_sweep(
    targets: list[Target],
    check: Checker,
    store: SweepStore,
    started_at: float,
    *,
    workers: int = 4,
    budget: float | None = None,
    clock: Callable[[], float] = time.monotonic,
    now: Callable[[], float] = time.time,
    log: Logger | None = None,
    on_healthy: Callable[[list[Target]], None] | None = None,
) -> SweepStats:
    """Check every target not yet recorded for this sweep, up to ``workers``
    at once, stopping after ``budget`` seconds of work (the rest is picked
    up by the next run). ``on_healthy`` gets each batch's healthy targets,
    for callers that want to tell other tools about them."""
    stats = SweepStats(targets=len(targets))
    done = store.done_since(started_at)
    todo = [t for t in targets if t.cap not in done]
    stats.already_done = len(targets) - len(todo)
    t0 = clock()
    batch_size = max(workers, 1) * 4
    executor = ThreadPoolExecutor(max_workers=workers) if workers > 1 else None
    try:
        for start in range(0, len(todo), batch_size):
            if budget is not None and clock() - t0 >= budget:
                stats.budget_stop = True
                break
            batch = todo[start : start + batch_size]
            if executor is None:
                results = [(t, _check(check, t)) for t in batch]
            else:
                futures = [(t, executor.submit(_check, check, t)) for t in batch]
                results = [(t, future.result()) for t, future in futures]
            healthy_now: list[Target] = []
            for target, outcome in results:
                if isinstance(outcome, Exception):
                    stats.failed += 1
                    if log:
                        log(f"check failed for {target.cap[:40]}...: {outcome!s:.120}")
                    continue
                stats.checked += 1
                store.record(target, outcome, now())
                if outcome.repair_attempted and outcome.repair_successful:
                    stats.repaired += 1
                if outcome.healthy:
                    stats.healthy += 1
                    healthy_now.append(target)
                else:
                    stats.unhealthy += 1
                    stats.unhealthy_caps.append(target.cap)
            if on_healthy and healthy_now:
                on_healthy(healthy_now)
            if log and stats.checked and stats.checked % 1000 < len(batch):
                log(f"{stats.checked}/{len(todo)} objects checked")
    finally:
        if executor is not None:
            executor.shutdown()
    stats.seconds = clock() - t0
    return stats
