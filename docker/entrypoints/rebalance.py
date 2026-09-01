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
  REDUNDANET_REBALANCE_ENABLED   default "true" (set "false" to disable)
  REDUNDANET_REBALANCE_INTERVAL  seconds between cycles (default 86400)
  REDUNDANET_REBALANCE_PAUSE     pause between files (default 10s)
  REDUNDANET_REBALANCE_BUDGET    max re-encoding seconds per cycle (default 4h)
"""

from __future__ import annotations

import contextlib
import json
import os
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path

NODE_DIR = "/var/lib/tahoe-client"
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


def parse_config(environ: dict[str, str]) -> RebalanceConfig:
    return RebalanceConfig(
        enabled=environ.get("REDUNDANET_REBALANCE_ENABLED", "true").lower() != "false",
        interval=_int_env(environ, "REDUNDANET_REBALANCE_INTERVAL", 86400),
        pause=_int_env(environ, "REDUNDANET_REBALANCE_PAUSE", 10),
        budget=_int_env(environ, "REDUNDANET_REBALANCE_BUDGET", 14400),
        needed=_int_env(environ, "REDUNDANET_SHARES_NEEDED", 3),
        total=_int_env(environ, "REDUNDANET_SHARES_TOTAL", 10),
    )


def parse_chk_params(cap: str) -> tuple[int, int] | None:
    """(k, n) of an immutable CHK capability, or None for anything else.

    CHK caps literally contain the encoding: URI:CHK:<key>:<hash>:<k>:<n>:<size>.
    LIT caps (tiny files inlined in the cap) have no shares and never need
    re-encoding; directories and mutables are out of scope here.
    """
    parts = cap.strip().split(":")
    if len(parts) < 7 or parts[0] != "URI" or parts[1] != "CHK":
        return None
    try:
        return int(parts[4]), int(parts[5])
    except ValueError:
        return None


def run_tahoe(args: list[str], timeout: int = 3600) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["tahoe", "-d", NODE_DIR, *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def list_aliases(run=run_tahoe) -> list[str]:
    result = run(["list-aliases"], timeout=60)
    if result.returncode != 0:
        return []
    return [line.split(":", 1)[0].strip() for line in result.stdout.splitlines() if ":" in line]


def walk_files(root: str, run=run_tahoe) -> list[tuple[str, str]]:
    """All (grid path, file cap) pairs reachable from ``root`` (an alias spec
    like ``backups:``), via recursive `tahoe ls --json` — structured output,
    no fragile text parsing. Directories themselves are skipped (v1 re-encodes
    immutable files only; directory objects are tiny mutables)."""
    files: list[tuple[str, str]] = []
    pending: list[str] = [""]
    while pending:
        subpath = pending.pop()
        spec = f"{root}{subpath}"
        result = run(["ls", "--json", spec], timeout=300)
        if result.returncode != 0:
            log(f"cannot list {spec!r} (skipping): {result.stderr.strip()[:120]}")
            continue
        try:
            node_type, payload = json.loads(result.stdout)
        except (ValueError, TypeError):
            log(f"unparseable listing for {spec!r} (skipping)")
            continue
        if node_type != "dirnode":
            continue
        for name, (child_type, child) in sorted((payload.get("children") or {}).items()):
            child_path = f"{subpath}/{name}" if subpath else name
            if child_type == "dirnode":
                pending.append(child_path)
            elif child_type == "filenode":
                cap = child.get("rw_uri") or child.get("ro_uri") or ""
                if cap:
                    files.append((child_path, cap))
    return files


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
    config = parse_config(dict(os.environ))
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
