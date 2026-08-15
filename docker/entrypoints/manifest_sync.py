#!/usr/bin/env python3
"""Periodic manifest re-sync sidecar for the RedundaNet tinc container.

Runs next to tincd under supervisord. Every REDUNDANET_SYNC_INTERVAL seconds
(default 300) it:

  1. re-syncs the manifest git repository,
  2. refreshes the Tinc peer host files from the manifest (adding host files
     for newly joined nodes, deleting those of removed nodes),
  3. rewrites tinc.conf's ConnectTo list and sends tincd a HUP so the changes
     take effect without restarting the container.

This is what makes membership changes propagate to running nodes: without it,
joins and revocations only took effect when every operator restarted their
tinc container.
"""

import os
import signal
import subprocess
import time
from pathlib import Path

import yaml

from redundanet.core.deployment import git_sync
from redundanet.core.manifest import locate_manifest
from redundanet.utils.logging import get_logger, setup_logging
from redundanet.vpn.peers import sync_peer_host_files, tinc_name
from redundanet.vpn.tinc import TincConfig, TincManager

MANIFEST_DIR = Path("/var/lib/redundanet/manifest")
TINC_CONFIG_DIR = Path("/etc/tinc/redundanet")


def hup_tincd() -> bool:
    """Ask the running tincd to reload its configuration and host files."""
    result = subprocess.run(
        ["pkill", f"-{signal.SIGHUP.name}", "-x", "tincd"],
        capture_output=True,
    )
    return result.returncode == 0


def run_once(
    node_name: str,
    repo: str,
    branch: str,
    manifest_dir: Path = MANIFEST_DIR,
    config_dir: Path = TINC_CONFIG_DIR,
    reload_tincd=hup_tincd,
) -> bool:
    """One sync pass. Returns True if the tinc configuration changed."""
    logger = get_logger()

    if repo:
        result = git_sync(repo, branch, manifest_dir)
        if not result.success:
            logger.warning("Manifest sync failed; keeping current peers",
                           error=result.stderr.strip())
            return False

    manifest_file = locate_manifest(manifest_dir)
    if manifest_file is None:
        logger.warning("No manifest.yaml present; nothing to sync")
        return False
    manifest = yaml.safe_load(manifest_file.read_text()) or {}
    nodes = manifest.get("nodes", [])

    hosts_dir = config_dir / "hosts"
    peers = sync_peer_host_files(nodes, node_name, hosts_dir, manifest_dir)
    if not peers.changed:
        return False

    logger.info(
        "Peer set changed",
        written=peers.written,
        removed=peers.removed,
        connect_to=peers.connect_to,
    )

    # Rewrite tinc.conf with the new ConnectTo list, then HUP tincd so it
    # rereads tinc.conf and the host files.
    self_entry = next((n for n in nodes if n.get("name") == node_name), {})
    config = TincConfig(
        network_name="redundanet",
        node_name=tinc_name(node_name),
        vpn_ip=os.environ.get("REDUNDANET_INTERNAL_VPN_IP", ""),
        port=int((self_entry.get("ports", {}) or {}).get("tinc", 655)),
        connect_to=peers.connect_to,
        config_dir=config_dir.parent,  # network_dir appends /redundanet
    )
    TincManager(config)._write_tinc_conf()

    if reload_tincd():
        logger.info("Sent HUP to tincd; new peer set is live")
    else:
        logger.warning("Could not signal tincd (not running yet?); config is on disk")
    return True


def main() -> None:
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()

    node_name = os.environ.get("REDUNDANET_NODE_NAME", "")
    repo = os.environ.get("REDUNDANET_MANIFEST_REPO", "")
    branch = os.environ.get("REDUNDANET_MANIFEST_BRANCH", "main")
    interval = int(os.environ.get("REDUNDANET_SYNC_INTERVAL", "300"))

    if not node_name:
        logger.error("REDUNDANET_NODE_NAME is required")
        raise SystemExit(1)
    if not repo:
        # Static manifest (e.g. mesh test mounts it read-only): nothing will
        # ever change, so idle instead of exiting and being restart-looped.
        logger.info("No manifest repository configured; manifest sync disabled")
        while True:
            time.sleep(3600)

    logger.info("Manifest sync started", interval_seconds=interval, repo=repo)
    while True:
        time.sleep(interval)
        try:
            run_once(node_name, repo, branch)
        except Exception as e:  # a bad sync pass must not kill the sidecar
            logger.warning("Manifest sync pass failed", error=str(e))


if __name__ == "__main__":
    main()
