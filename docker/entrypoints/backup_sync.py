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
  REDUNDANET_SYNC_EXCLUDE   comma-separated glob patterns matched against file
                            and directory names, passed to tahoe backup as
                            --exclude (default none)
  REDUNDANET_SYNC_REENCODE  "true" (default) to forget backupdb entries made
                            at an older k-of-n before each run, so the files
                            are re-uploaded at the node's current encoding
  REDUNDANET_SYNC_MAX_AGE   seconds after which a run happens even if the
                            share is unchanged (default 86400): one snapshot
                            a day as a heartbeat, not one every interval

A run is skipped when the share has not changed since the last one: ``tahoe
backup`` would upload nothing but still rewrite the mutable Archives/
directory (four shares of a listing that grows by one entry per run) and
link one more identical snapshot. The share is fingerprinted with one walk
(paths, sizes, mtimes), a fraction of what the backup itself walks.

Symlinks and special files are skipped by tahoe backup itself (it has no
option to follow them); the run still succeeds and the skipped paths are
logged. Anything a symlink points at outside the sync directory is invisible
in the container anyway: mount it (docs/nas-backup.md).

Snapshots accumulate on purpose (oops/ransomware protection); pruning old
Archives/ is a deliberate future feature tied to the lease/GC policy work.
"""

from __future__ import annotations

import contextlib
import hashlib
import json
import os
import sqlite3
import subprocess
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path

from redundanet.core.quota import node_encoding
from redundanet.monitor.usage import USAGE_FILE, load_usage_file, over_allocation
from redundanet.storage.backupdb import BACKUPDB_FILE, prune_stale

NODE_DIR = "/var/lib/tahoe-client"
TAHOE_CFG = Path(NODE_DIR) / "tahoe.cfg"
# What the last run saw (fingerprint, time), so an unchanged share skips runs.
STATE_FILE = Path(NODE_DIR) / "redundanet-sync-state.json"
# Touched after a run that made a snapshot: the usage meter measures again.
BACKUP_DONE_FILE = Path(NODE_DIR) / "redundanet-backup-done"
# tahoe backup's exit status when the snapshot was made but entries were
# skipped (symlinks, special files, unreadable paths).
EXIT_SKIPPED = 2
MAX_SKIPPED_LOGGED = 20
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
    exclude: list[str] = field(default_factory=list)
    reencode: bool = True
    max_age: int = 86400


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
        exclude=[
            pattern.strip()
            for pattern in environ.get("REDUNDANET_SYNC_EXCLUDE", "").split(",")
            if pattern.strip()
        ],
        reencode=environ.get("REDUNDANET_SYNC_REENCODE", "true").lower() != "false",
        max_age=_int_env(environ, "REDUNDANET_SYNC_MAX_AGE", 86400),
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


def quota_blocks(usage_file: Path = USAGE_FILE) -> bool:
    """Whether the member's allocation is used up (and the network enforces it).

    Reads the usage meter's last report; with no report the sync proceeds
    (an unknown quota must not silently stop backups). The share keeps
    working either way; only the copy into the grid pauses, and it resumes on
    its own once usage drops below the allocation.
    """
    payload = load_usage_file(usage_file)
    if payload is None or not over_allocation(payload):
        return False
    log(
        f"over allocation: {payload.get('member')} uses {payload.get('used_bytes')} of "
        f"{payload.get('allocation_bytes')} bytes; skipping this run until usage drops "
        "(delete data, or contribute more storage)"
    )
    return True


def tree_fingerprint(sync_dir: str) -> str:
    """A digest of every entry's path, size and mtime under the share (one
    scandir walk, symlinks not followed). Any create, delete, rename, write
    or touch changes it."""
    digest = hashlib.sha256()

    def walk(path: str) -> None:
        try:
            with os.scandir(path) as entries:
                children = sorted(entries, key=lambda e: e.name)
        except OSError:
            return
        for entry in children:
            try:
                if entry.is_dir(follow_symlinks=False):
                    digest.update(f"d:{entry.path}\0".encode(errors="surrogateescape"))
                    walk(entry.path)
                elif entry.is_file(follow_symlinks=False):
                    st = entry.stat(follow_symlinks=False)
                    digest.update(
                        f"f:{entry.path}:{st.st_size}:{st.st_mtime_ns}\0".encode(
                            errors="surrogateescape"
                        )
                    )
            except OSError:
                continue

    walk(sync_dir)
    return digest.hexdigest()


def load_state(path: Path = STATE_FILE) -> dict:
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def save_state(state: dict, path: Path = STATE_FILE) -> None:
    try:
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(state))
        tmp.replace(path)
    except OSError as e:
        log(f"cannot write {path}: {e}")


def unchanged_since_last_run(
    config: SyncConfig, fingerprint: str, state: dict, now: float | None = None
) -> bool:
    """Whether this run can be skipped: same fingerprint as the last successful
    run, and that run is younger than max_age (a daily snapshot still happens)."""
    now = time.time() if now is None else now
    last_at = state.get("last_run_at")
    if state.get("fingerprint") != fingerprint or not isinstance(last_at, int | float):
        return False
    return now - last_at < config.max_age


def forget_stale_encoding(
    config: SyncConfig, tahoe_cfg: Path = TAHOE_CFG, db_path: Path = BACKUPDB_FILE
) -> None:
    """Before a run: drop backupdb rows made at another k-of-n than the node's.

    The comparison is against the running node's ``tahoe.cfg``, not the
    manifest: Tahoe applies a new encoding only when the container is
    recreated, so this fires exactly once, at the first run after that, and
    can never re-upload at an encoding the node cannot produce.
    """
    if not config.reencode:
        return
    encoding = node_encoding(tahoe_cfg)
    if encoding is None:
        log(f"cannot read the node's encoding from {tahoe_cfg}; keeping the backupdb as is")
        return
    try:
        pruned = prune_stale(db_path, *encoding)
    except sqlite3.Error as e:  # locked or damaged: the backup run matters more
        log(f"backupdb: cannot prune ({e}); keeping it as is for this run")
        return
    if pruned:
        log(
            f"backupdb: forgot {pruned.files} files and {pruned.directories} directories "
            f"made at an older encoding; re-uploading them at {encoding[0]}-of-{encoding[1]}"
        )


def skipped_paths(stderr: str) -> list[str]:
    """The entries tahoe backup refused, from its stderr warnings, e.g.
    ``WARNING: cannot backup symlink 'photos'``."""
    return [
        line.strip()[len("WARNING: ") :]
        for line in stderr.splitlines()
        if line.strip().startswith("WARNING: ")
    ]


def backup_args(config: SyncConfig) -> list[str]:
    args = ["backup"]
    for pattern in config.exclude:
        args.append(f"--exclude={pattern}")
    return [*args, config.sync_dir, f"{config.alias}:"]


def run_backup(config: SyncConfig, run=run_tahoe) -> bool:
    """One incremental backup pass. Returns True when a snapshot was made."""
    if not has_content(config.sync_dir):
        log(f"{config.sync_dir} is missing or empty; nothing to back up")
        return True
    started = time.monotonic()
    result = run(backup_args(config), timeout=config.timeout)
    elapsed = int(time.monotonic() - started)
    if result.returncode not in (0, EXIT_SKIPPED):
        log(f"backup FAILED after {elapsed}s (will retry next cycle): {result.stderr.strip()}")
        return False
    # tahoe backup summarizes what it did on stdout's last line.
    summary = result.stdout.strip().splitlines()[-1] if result.stdout.strip() else "done"
    if result.returncode == EXIT_SKIPPED:
        # The snapshot exists and Latest points at it; some entries were left
        # out (symlinks and special files are never backed up, unreadable
        # paths cannot be). Name them so the operator can mount or exclude.
        skipped = skipped_paths(result.stderr)
        log(f"backup ok in {elapsed}s with {len(skipped)} skipped: {summary}")
        for entry in skipped[:MAX_SKIPPED_LOGGED]:
            log(f"  skipped: {entry}")
        if len(skipped) > MAX_SKIPPED_LOGGED:
            log(f"  ... and {len(skipped) - MAX_SKIPPED_LOGGED} more")
        return True
    log(f"backup ok in {elapsed}s: {summary}")
    return True


def main() -> None:
    config = parse_config(dict(os.environ))
    if not config.enabled:
        log("disabled (set SYNC_ENABLED=true in .env to enable); sleeping")
        while True:  # sleep forever without supervisord restart churn
            time.sleep(3600)

    log(
        f"enabled: backing up {config.sync_dir} to {config.alias}: every {config.interval}s "
        f"when the share changed, at least every {config.max_age}s"
    )
    time.sleep(STARTUP_DELAY)
    while True:
        try:
            if not quota_blocks() and ensure_alias(config.alias):
                fingerprint = tree_fingerprint(config.sync_dir)
                state = load_state()
                if unchanged_since_last_run(config, fingerprint, state):
                    log("share unchanged since the last snapshot; skipping this run")
                else:
                    forget_stale_encoding(config)
                    if run_backup(config):
                        save_state({"fingerprint": fingerprint, "last_run_at": time.time()})
                        with contextlib.suppress(OSError):
                            BACKUP_DONE_FILE.touch()
        except subprocess.TimeoutExpired:
            log("backup timed out; will retry next cycle")
        except Exception as e:  # never die: the loop must survive transient errors
            log(f"unexpected error (will retry next cycle): {e}")
        time.sleep(config.interval)


if __name__ == "__main__":
    sys.exit(main())
