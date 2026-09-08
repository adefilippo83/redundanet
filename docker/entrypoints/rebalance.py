#!/usr/bin/env python3
"""Automatic re-encoder: converge stored files to the current erasure coding.

Encoding parameters (k-of-n) are baked into each immutable file at upload
time, so changing them in the manifest affects only new uploads. This loop
closes the gap: it walks every alias, detects files whose capability carries
old parameters (k and n are literal fields inside a CHK URI), and re-encodes
them by downloading from the grid and re-uploading at the current parameters,
relinking the same path. Nobody needs the original file locally: the grid is
the source.

Properties:
  * a strict no-op when everything already matches (detection needs no
    downloads, just `tahoe ls --json` walks and string parsing)
  * serial and rate-limited, with a per-cycle time budget: a large archive
    converges over days without saturating home uplinks
  * idempotent and crash-safe: convergent encryption means re-encoding the
    same content yields the same new cap, and a rerun simply continues
  * replaced caps stop being lease-renewed (the renewer walks live aliases),
    so old shares age out via GC on their own

Environment:
  REDUNDANET_SHARES_NEEDED / REDUNDANET_SHARES_TOTAL   the target encoding
      (fallback: the synced manifest's network.tahoe section wins when present)
  REDUNDANET_REBALANCE_ENABLED   default "true" (set "false" to disable)
  REDUNDANET_REBALANCE_INTERVAL  seconds between cycles (default 86400)
  REDUNDANET_REBALANCE_PAUSE     pause between files (default 10s)
  REDUNDANET_REBALANCE_BUDGET    max re-encoding seconds per cycle (default 4h)
"""

from __future__ import annotations

import contextlib
import os
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path

from redundanet.core.manifest import read_manifest
from redundanet.core.quota import resolve_encoding
from redundanet.storage import inventory

NODE_DIR = "/var/lib/tahoe-client"
MANIFEST_DIR = Path("/var/lib/redundanet/manifest")
TMP_FILE = Path("/tmp/rebalance.tmp")  # noqa: S108 - private container tmp
STARTUP_DELAY = 180  # let the client connect to the grid first


def log(message: str) -> None:
    print(f"rebalance: {message}", flush=True)


@dataclass
class RebalanceConfig:
    enabled: bool
    interval: int
    pause: int
    budget: int
    needed: int
    total: int


def _int_env(environ: dict[str, str], name: str, default: int) -> int:
    raw = environ.get(name, "")
    try:
        return int(raw) if raw else default
    except ValueError:
        log(f"invalid {name}={raw!r}; using default {default}")
        return default


def parse_config(environ: dict[str, str], manifest: dict | None = None) -> RebalanceConfig:
    """The target encoding comes from the synced manifest when present (the
    network's source of truth), else from the REDUNDANET_SHARES_* variables."""
    needed, _happy, total = resolve_encoding(manifest or {}, environ)
    return RebalanceConfig(
        enabled=environ.get("REDUNDANET_REBALANCE_ENABLED", "true").lower() != "false",
        interval=_int_env(environ, "REDUNDANET_REBALANCE_INTERVAL", 86400),
        pause=_int_env(environ, "REDUNDANET_REBALANCE_PAUSE", 10),
        budget=_int_env(environ, "REDUNDANET_REBALANCE_BUDGET", 14400),
        needed=needed,
        total=total,
    )


# Capability parsing and the alias walk live in redundanet.storage.inventory
# (shared with the usage meter); kept under their historical names here.
parse_chk_params = inventory.parse_chk_params


def run_tahoe(args: list[str], timeout: int = 3600) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["tahoe", "-d", NODE_DIR, *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def list_aliases(run=run_tahoe) -> list[str]:
    return inventory.list_aliases(run)


def walk_files(root: str, run=run_tahoe) -> list[tuple[str, str]]:
    """All (grid path, file cap) pairs under an alias spec like ``backups:``.
    Directories are walked, not returned. Immutable directories (backup
    snapshots) are skipped: a re-encoded file cannot be relinked inside them,
    so trying would download and fail on every cycle."""
    return inventory.walk_files(root, run, log=log, skip_immutable=True)


def reencode_file(root: str, path: str, cap: str, run=run_tahoe) -> bool:
    """Download by cap, re-upload at current parameters, relink the path."""
    try:
        got = run(["get", cap, str(TMP_FILE)])
        if got.returncode != 0:
            log(f"get failed for {path!r}: {got.stderr.strip()[:120]}")
            return False
        put = run(["put", str(TMP_FILE), f"{root}{path}"])
        if put.returncode != 0:
            log(f"put failed for {path!r}: {put.stderr.strip()[:120]}")
            return False
        return True
    finally:
        with contextlib.suppress(OSError):
            TMP_FILE.unlink(missing_ok=True)


def run_cycle(
    config: RebalanceConfig,
    run=run_tahoe,
    sleep=time.sleep,
    clock=time.monotonic,
) -> dict[str, int]:
    """One pass over all aliases. Returns counters for logging/tests."""
    stats = {"scanned": 0, "mismatched": 0, "reencoded": 0, "failed": 0, "budget_stop": 0}
    target = (config.needed, config.total)
    started = clock()
    for alias in list_aliases(run=run):
        root = f"{alias}:"
        for path, cap in walk_files(root, run=run):
            stats["scanned"] += 1
            params = parse_chk_params(cap)
            if params is None or params == target:
                continue
            stats["mismatched"] += 1
            if clock() - started > config.budget:
                stats["budget_stop"] = 1
                log("cycle budget reached; will continue next cycle")
                return stats
            log(
                f"re-encoding {alias}:{path} {params[0]}-of-{params[1]} -> "
                f"{target[0]}-of-{target[1]}"
            )
            if reencode_file(root, path, cap, run=run):
                stats["reencoded"] += 1
            else:
                stats["failed"] += 1
            sleep(config.pause)
    return stats


def main() -> None:
    config = parse_config(dict(os.environ), read_manifest(MANIFEST_DIR))
    if not config.enabled:
        log("disabled (REBALANCE_ENABLED=false); sleeping")
        while True:
            time.sleep(3600)
    if config.needed > config.total or config.needed < 1:
        log(f"invalid target encoding {config.needed}-of-{config.total}; sleeping")
        while True:
            time.sleep(3600)

    log(
        f"active: target {config.needed}-of-{config.total}, "
        f"cycle every {config.interval}s, budget {config.budget}s"
    )
    time.sleep(STARTUP_DELAY)
    while True:
        try:
            # Re-read the target each cycle: the manifest syncs every few minutes,
            # so an encoding change is picked up without a restart.
            config = parse_config(dict(os.environ), read_manifest(MANIFEST_DIR))
            stats = run_cycle(config)
            if stats["mismatched"] or stats["failed"]:
                log(
                    f"cycle done: {stats['scanned']} scanned, "
                    f"{stats['reencoded']} re-encoded, {stats['failed']} failed"
                )
            else:
                log(f"cycle done: {stats['scanned']} scanned, all at target encoding")
        except subprocess.TimeoutExpired:
            log("a tahoe command timed out; will retry next cycle")
        except Exception as e:  # the loop must survive anything transient
            log(f"unexpected error (will retry next cycle): {e}")
        time.sleep(config.interval)


if __name__ == "__main__":
    sys.exit(main())
