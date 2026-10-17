#!/usr/bin/env python3
"""Lease renewal and repair sweep for the tahoe-client container.

Storage nodes garbage-collect shares whose lease is older than the network's
lease duration (expire.override_lease_duration, default 90 days). This job
renews the leases of everything this client keeps on the grid, once per
interval, and repairs objects with missing shares (a storage node lost its
disk, a member left): Tahoe re-derives the missing shares from the surviving
ones and places them on servers that lack one. Only objects that still have
at least k shares can be repaired; below k they are gone.

What is swept: every file and directory reachable from the client's aliases
(each object once, however many snapshots link it), plus the files a backup
in progress has uploaded but not linked into a snapshot yet (their only
record is the backup database). Objects are checked through the node's web
API several at a time, and every result is recorded in
/var/lib/tahoe-client/redundanet-sweep.sqlite, so a sweep interrupted by a
restart or by its time budget resumes where it stopped. Files and
directories found healthy are also marked as checked in the backup
database, so ``tahoe backup`` does not spend its own runs re-checking them
once they are two months old.

Repair is on by default; REDUNDANET_REPAIR_ENABLED=false only renews.
Bare capabilities that are not linked into any alias are neither renewed nor
repaired here; their owner must do it (redundanet storage renew <cap>).

Environment:
  REDUNDANET_LEASE_RENEW_INTERVAL  seconds between sweeps (default 604800, 7 days)
  REDUNDANET_LEASE_RENEW_TIMEOUT   seconds of checking per run (default 21600, 6h);
                                   a sweep that hits it pauses a minute and resumes
  REDUNDANET_REPAIR_ENABLED        "true" (default) repairs; "false" only renews
  REDUNDANET_SWEEP_WORKERS         checks in flight at once (default 4)

``--once`` runs one sweep now (resuming an unfinished one) and exits.
"""

from __future__ import annotations

import contextlib
import os
import sqlite3
import subprocess
import sys
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from redundanet.storage.backupdb import BACKUPDB_FILE, mark_checked, recorded_caps
from redundanet.storage.dircache import DirCache
from redundanet.storage.inventory import Lister, alias_caps, read_node_url, web_lister
from redundanet.storage.sweep import (
    Checker,
    SweepStats,
    SweepStore,
    Target,
    collect_targets,
    run_sweep,
    web_checker,
)

NODE_DIR = Path("/var/lib/tahoe-client")
NODE_URL_FILE = NODE_DIR / "node.url"
SWEEP_DB = NODE_DIR / "redundanet-sweep.sqlite"
DIRCACHE_FILE = NODE_DIR / "redundanet-dircache.sqlite"
# When the last sweep finished, as the previous shell job recorded it; still
# written, and read when the store has no finished sweep yet, so an update
# keeps the schedule instead of starting a sweep at once.
LEGACY_STATE_FILE = NODE_DIR / "redundanet-lease-last-run"
# Give the client time to connect to the grid after a (re)start.
STARTUP_DELAY = 300
RESUME_PAUSE = 60
DEFAULT_INTERVAL = 604800
DEFAULT_TIMEOUT = 21600
DEFAULT_WORKERS = 4


def log(message: str) -> None:
    print(f"lease-repair: {message}", flush=True)


@dataclass(frozen=True)
class SweepConfig:
    interval: int
    timeout: int
    repair: bool
    workers: int


def _int_env(environ: dict[str, str], name: str, default: int) -> int:
    raw = environ.get(name, "")
    try:
        return int(raw) if raw else default
    except ValueError:
        log(f"invalid {name}={raw!r}; using default {default}")
        return default


def parse_config(environ: dict[str, str]) -> SweepConfig:
    return SweepConfig(
        interval=_int_env(environ, "REDUNDANET_LEASE_RENEW_INTERVAL", DEFAULT_INTERVAL),
        timeout=_int_env(environ, "REDUNDANET_LEASE_RENEW_TIMEOUT", DEFAULT_TIMEOUT),
        repair=environ.get("REDUNDANET_REPAIR_ENABLED", "true").lower() != "false",
        workers=max(1, _int_env(environ, "REDUNDANET_SWEEP_WORKERS", DEFAULT_WORKERS)),
    )


def run_tahoe(args: list[str], timeout: int = 3600) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["tahoe", "-d", str(NODE_DIR), *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def open_dircache(path: Path = DIRCACHE_FILE) -> DirCache:
    try:
        return DirCache(path)
    except sqlite3.Error as e:
        log(f"cannot open the directory cache {path}: {e}; listing everything")
        return DirCache()


def seconds_until_due(
    store: SweepStore, interval: int, now: float, legacy: Path = LEGACY_STATE_FILE
) -> float:
    """How long until the next sweep is due. Zero when none ever finished."""
    last = store.last_finished_at()
    if last is None:
        try:
            last = float(legacy.read_text().strip())
        except (OSError, ValueError):
            return 0.0
    return max(last + interval - now, 0.0)


def record_legacy(path: Path = LEGACY_STATE_FILE, now: float | None = None) -> None:
    now = time.time() if now is None else now
    with contextlib.suppress(OSError):
        path.write_text(f"{now:.0f}\n")


def sweep_once(
    config: SweepConfig,
    store: SweepStore,
    *,
    run: Callable[..., subprocess.CompletedProcess[str]] = run_tahoe,
    list_dir: Lister | None = None,
    check: Checker | None = None,
    cache: DirCache | None = None,
    backupdb: Path = BACKUPDB_FILE,
    now: Callable[[], float] = time.time,
    clock: Callable[[], float] = time.monotonic,
    legacy: Path = LEGACY_STATE_FILE,
) -> SweepStats:
    """Run, or resume, one sweep. Returns its stats; ``budget_stop`` means
    it paused and a later call continues it."""
    current = store.current()
    if current is None:
        sweep_id, started_at = store.start(now())
        log(f"sweep started ({'renew+repair' if config.repair else 'renew only'})")
    else:
        sweep_id, started_at = current
        log(f"resuming the sweep started {int(now() - started_at)}s ago")

    node_url = read_node_url(NODE_URL_FILE)
    list_dir = list_dir or web_lister(node_url)
    check = check or web_checker(node_url, repair=config.repair)
    cache = cache or open_dircache()
    aliases = alias_caps(run)
    skipped: list[str] = []
    pending = recorded_caps(backupdb)
    targets = collect_targets(
        aliases,
        list_dir,
        pending_caps=pending,
        cache=cache,
        log=log,
        skipped=skipped,
        workers=config.workers,
    )
    kinds = {kind: sum(1 for t in targets if t.kind == kind) for kind in ("dir", "file", "pending")}
    log(
        f"{len(targets)} objects across {len(aliases)} aliases: {kinds['dir']} directories, "
        f"{kinds['file']} files, {kinds['pending']} uploads not linked yet"
        + (f"; {len(skipped)} directories could not be listed" if skipped else "")
    )

    def on_healthy(batch: list[Target]) -> None:
        files = [t.cap for t in batch if t.kind != "dir"]
        dirs = [t.cap for t in batch if t.kind == "dir"]
        try:
            mark_checked(backupdb, files, dirs, now())
        except sqlite3.Error as e:
            log(f"backupdb: could not record the checks ({e}); tahoe backup will re-check")

    stats = run_sweep(
        targets,
        check,
        store,
        started_at,
        workers=config.workers,
        budget=config.timeout,
        clock=clock,
        now=now,
        log=log,
        on_healthy=on_healthy,
    )
    if stats.budget_stop:
        log(stats.summary())
        return stats
    store.finish(sweep_id, now(), stats)
    record_legacy(legacy, now())
    log("sweep done: " + stats.summary())
    for cap in stats.unhealthy_caps[:5]:
        log(f"unhealthy after the sweep: {cap[:60]}...")
    if skipped:
        log(f"not swept (listing failed): {', '.join(skipped[:3])}")
    return stats


def wait_for_node(
    node_url: Path = NODE_URL_FILE, sleep: Callable[[float], None] = time.sleep, poll: float = 10
) -> None:
    """Block until the client node exists. ``tahoe create-client`` refuses a
    node directory that is not empty, so nothing may be written there (the
    sweep's own database included) before the entrypoint has created the
    node on a fresh volume."""
    while not node_url.is_file():
        sleep(poll)


def main() -> int:
    config = parse_config(dict(os.environ))
    if "--once" in sys.argv:
        wait_for_node()
        stats = sweep_once(config, SweepStore(SWEEP_DB))
        return 0 if stats.complete else 1

    log(
        f"active: every {config.interval}s, up to {config.timeout}s per run, "
        f"{config.workers} checks at once, repair={'on' if config.repair else 'off'}"
    )
    wait_for_node()
    time.sleep(STARTUP_DELAY)
    store = SweepStore(SWEEP_DB)
    while True:
        try:
            if store.current() is None:
                wait = seconds_until_due(store, config.interval, time.time())
                if wait > 0:
                    log(f"next sweep in {int(wait)}s")
                    time.sleep(min(wait, 3600))
                    continue
            stats = sweep_once(config, store)
            if stats.budget_stop:
                time.sleep(RESUME_PAUSE)
                continue
            if stats.failed:
                log(f"{stats.failed} objects could not be checked; next sweep retries them")
        except Exception as e:  # the loop must survive anything transient
            log(f"sweep failed (will retry): {e}")
            time.sleep(RESUME_PAUSE * 5)


if __name__ == "__main__":
    sys.exit(main())
