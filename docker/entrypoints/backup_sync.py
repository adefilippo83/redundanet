#!/usr/bin/env python3
"""Async one-way backup of a local directory into the grid (tahoe backup).

The NAS pattern: the host exposes a directory over Samba/whatever it likes,
backed by a real local disk (fast, normal filesystem semantics), and this loop
periodically snapshots it into the grid with ``tahoe backup`` — incremental
(unchanged files are skipped via the backupdb), immutable, timestamped
snapshots under ``<alias>:Archives/`` plus a ``<alias>:Latest/`` link.

One-way by design: the share is the source of truth; the grid is the
replicated archive. (Bidirectional sync means conflict resolution — upstream's
"Magic Folder" attempted that and was abandoned.)

Environment:
  REDUNDANET_SYNC_ENABLED   "true" to enable (default "false": sleep forever)
  REDUNDANET_SYNC_INTERVAL  seconds between backup runs (default 900 = 15 min)
  REDUNDANET_SYNC_DIR       directory to back up (default /data/sync)
  REDUNDANET_SYNC_ALIAS     tahoe alias for the backups (default "backups")
  REDUNDANET_SYNC_TIMEOUT   per-run ceiling in seconds (default 21600 = 6h;
                            large initial syncs are legitimately slow)

Snapshots accumulate on purpose (oops/ransomware protection); pruning old
Archives/ is a deliberate future feature tied to the lease/GC policy work.
"""

from __future__ import annotations

import os
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path

NODE_DIR = "/var/lib/tahoe-client"
# Give the client time to connect to the grid after a (re)start before the
# first backup attempt (same pattern as lease_renew.sh).
STARTUP_DELAY = 120


def log(message: str) -> None:
    print(f"backup-sync: {message}", flush=True)


@dataclass
class SyncConfig:
    enabled: bool
    interval: int
    sync_dir: str
    alias: str
    timeout: int


def _int_env(environ: dict[str, str], name: str, default: int) -> int:
    """Parse an integer env var; a bad value logs and falls back to the
    default instead of crash-looping the whole program under supervisord."""
    raw = environ.get(name, "")
    try:
        return int(raw) if raw else default
    except ValueError:
        log(f"invalid {name}={raw!r}; using default {default}")
        return default


def parse_config(environ: dict[str, str]) -> SyncConfig:
    """Read the sync configuration from environment variables."""
    return SyncConfig(
        enabled=environ.get("REDUNDANET_SYNC_ENABLED", "false").lower() == "true",
        interval=_int_env(environ, "REDUNDANET_SYNC_INTERVAL", 900),
        sync_dir=environ.get("REDUNDANET_SYNC_DIR", "/data/sync"),
        alias=environ.get("REDUNDANET_SYNC_ALIAS", "backups"),
        # A large first sync (hundreds of GB over erasure coding + VPN) can
        # legitimately run for hours. Progress survives a timeout (the
        # backupdb records completed files), but killing a run mid-file
        # wastes work — so the ceiling is generous.
        timeout=_int_env(environ, "REDUNDANET_SYNC_TIMEOUT", 21600),  # 6h
    )


def run_tahoe(args: list[str], timeout: int = 3600) -> subprocess.CompletedProcess[str]:
    """Run a tahoe CLI command against the local client node."""
    return subprocess.run(
        ["tahoe", "-d", NODE_DIR, *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def ensure_alias(alias: str, run=run_tahoe) -> bool:
    """Make sure the backup alias exists, creating it on first use."""
    listing = run(["list-aliases"], timeout=60)
    if listing.returncode != 0:
        log(f"cannot list aliases (client not ready?): {listing.stderr.strip()}")
        return False
    names = [line.split(":", 1)[0].strip() for line in listing.stdout.splitlines() if ":" in line]
    if alias in names:
        return True
    log(f"creating alias {alias}:")
    created = run(["create-alias", alias], timeout=120)
    if created.returncode != 0:
        log(f"failed to create alias {alias}: {created.stderr.strip()}")
        return False
    return True


def has_content(sync_dir: str) -> bool:
    """Whether the sync directory exists and holds anything to back up."""
    path = Path(sync_dir)
    if not path.is_dir():
        return False
    try:
        next(path.iterdir())
        return True
    except StopIteration:
        return False


def run_backup(config: SyncConfig, run=run_tahoe) -> bool:
    """One incremental backup pass. Returns True on success."""
    if not has_content(config.sync_dir):
        log(f"{config.sync_dir} is missing or empty; nothing to back up")
        return True
    started = time.monotonic()
    result = run(["backup", config.sync_dir, f"{config.alias}:"], timeout=config.timeout)
    elapsed = int(time.monotonic() - started)
    if result.returncode != 0:
        log(f"backup FAILED after {elapsed}s (will retry next cycle): {result.stderr.strip()}")
        return False
    # tahoe backup summarizes what it did on stdout's last line.
    summary = result.stdout.strip().splitlines()[-1] if result.stdout.strip() else "done"
    log(f"backup ok in {elapsed}s: {summary}")
    return True


def main() -> None:
    config = parse_config(dict(os.environ))
    if not config.enabled:
        log("disabled (set SYNC_ENABLED=true in .env to enable); sleeping")
        while True:  # sleep forever without supervisord restart churn
            time.sleep(3600)

    log(f"enabled: backing up {config.sync_dir} to {config.alias}: every {config.interval}s")
    time.sleep(STARTUP_DELAY)
    while True:
        try:
            if ensure_alias(config.alias):
                run_backup(config)
        except subprocess.TimeoutExpired:
            log("backup timed out; will retry next cycle")
        except Exception as e:  # never die: the loop must survive transient errors
            log(f"unexpected error (will retry next cycle): {e}")
        time.sleep(config.interval)


if __name__ == "__main__":
    sys.exit(main())
