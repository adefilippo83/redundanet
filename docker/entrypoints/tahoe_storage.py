#!/usr/bin/env python3
"""Tahoe-LAFS Storage container entrypoint for RedundaNet."""

import os
import subprocess
import sys
import time
from pathlib import Path

from redundanet.utils.logging import setup_logging, get_logger
from redundanet.storage.introducers import dedupe, introducer_furls_from_manifest
from redundanet.storage.storage import TahoeStorage, TahoeStorageConfig

# Ports used inside the (shared) tinc network namespace. Introducer/storage/client
# all live in the same netns when using `network_mode: service:tinc`, so these
# must not collide.
TUB_PORT = 3457
WEB_PORT = 4457


def wait_for_vpn(vpn_ip: str, timeout: int = 300) -> bool:
    """Wait until the VPN IP is assigned to a local interface."""
    logger = get_logger()
    start_time = time.time()

    while time.time() - start_time < timeout:
        try:
            result = subprocess.run(
                ["ip", "-o", "addr", "show"],
                capture_output=True,
                text=True,
            )
            if result.returncode == 0 and vpn_ip in result.stdout:
                logger.info("VPN interface is available", vpn_ip=vpn_ip)
                return True
        except Exception:
            pass

        logger.debug("Waiting for VPN interface...", vpn_ip=vpn_ip)
        time.sleep(5)

    logger.error("Timeout waiting for VPN interface", vpn_ip=vpn_ip)
    return False


def get_introducer_furls() -> list[str]:
    """Every introducer FURL this node should use, primary first.

    Sources, in precedence order: REDUNDANET_INTRODUCER_FURL (an explicit
    override, used alone); the FURL a local introducer container published to
    the shared manifest volume; then the manifest's top-level introducer_furl
    and each introducer node's own introducer_furl. The node announces itself
    to all of them, so any single introducer can be down.
    """
    logger = get_logger()

    furl = os.environ.get("REDUNDANET_INTRODUCER_FURL")
    if furl:
        return [furl.strip()]

    furls: list[str] = []
    manifest_dir = Path("/var/lib/redundanet/manifest")
    furl_file = manifest_dir / "introducer.furl"
    if furl_file.exists():
        local = furl_file.read_text().strip()
        if local:
            logger.info("Found introducer FURL in manifest volume")
            furls.append(local)

    # The manifest dir may be a plain dir or a full repo clone (manifests/manifest.yaml).
    from redundanet.core.manifest import locate_manifest

    manifest_file = locate_manifest(manifest_dir)
    if manifest_file is not None:
        import yaml

        with manifest_file.open() as f:
            manifest = yaml.safe_load(f) or {}
        from_manifest = introducer_furls_from_manifest(manifest)
        if from_manifest:
            logger.info("Found introducer FURLs in manifest.yaml", count=len(from_manifest))
        furls.extend(from_manifest)

    furls = dedupe(furls)
    if not furls:
        logger.warning("No introducer FURL found")
    return furls


def main():
    """Set up the storage node configuration, then exit.

    Supervisord then starts the actual `tahoe run` process.
    """
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()

    logger.info("Setting up RedundaNet Tahoe Storage")

    node_name = os.environ.get("REDUNDANET_NODE_NAME")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP")
    reserved_space = os.environ.get("REDUNDANET_RESERVED_SPACE", "1G")
    shares_needed = int(os.environ.get("REDUNDANET_SHARES_NEEDED", "3"))
    shares_happy = int(os.environ.get("REDUNDANET_SHARES_HAPPY", "7"))
    shares_total = int(os.environ.get("REDUNDANET_SHARES_TOTAL", "10"))
    expire_enabled = os.environ.get("REDUNDANET_EXPIRE_ENABLED", "true").lower() == "true"
    lease_duration = os.environ.get("REDUNDANET_LEASE_DURATION", "90 days")
    test_mode = os.environ.get("REDUNDANET_TEST_MODE", "false").lower() == "true"

    if not node_name:
        logger.error("REDUNDANET_NODE_NAME environment variable is required")
        sys.exit(1)

    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP environment variable is required")
        sys.exit(1)

    storage_dir = Path("/var/lib/tahoe-storage")
    storage_data_dir = Path("/data/storage")

    # Fresh ext4 volumes contain lost+found, which makes `tahoe create-node`
    # refuse the "non-empty" base directory (see tahoe_introducer.py).
    lost_found = storage_dir / "lost+found"
    if lost_found.is_dir():
        try:
            lost_found.rmdir()
            logger.info("Removed empty lost+found from fresh volume")
        except OSError:
            logger.warning("lost+found is not empty; not touching it")

    # Wait for VPN to be available (skip in test mode)
    if not test_mode:
        if not wait_for_vpn(vpn_ip):
            logger.error("VPN not available, cannot start storage node")
            sys.exit(1)
    else:
        logger.info("Test mode: skipping VPN wait")

    # Get the introducer FURLs (retry with backoff)
    introducer_furls: list[str] = []
    for attempt in range(30):
        introducer_furls = get_introducer_furls()
        if introducer_furls:
            break
        logger.info("Waiting for introducer FURL...", attempt=attempt + 1)
        time.sleep(10)

    if not introducer_furls:
        logger.error("Could not obtain introducer FURL")
        sys.exit(1)

    config = TahoeStorageConfig(
        nickname=f"{node_name}-storage",
        node_dir=storage_dir,
        introducer_furl=introducer_furls[0],
        extra_introducer_furls=introducer_furls[1:],
        reserved_space=reserved_space,
        storage_dir=storage_data_dir,
        web_port=WEB_PORT,
        tub_port=TUB_PORT,
        tub_location=f"tcp:{vpn_ip}:{TUB_PORT}",
        shares_needed=shares_needed,
        shares_happy=shares_happy,
        shares_total=shares_total,
        expire_enabled=expire_enabled,
        lease_duration=lease_duration,
    )
    storage = TahoeStorage(config)

    if not storage.is_configured():
        logger.info("Creating new Tahoe storage node", node=node_name)
        storage.create_node()
    else:
        logger.info("Using existing Tahoe storage configuration")
        storage.update_introducers(introducer_furls)

    logger.info("Tahoe storage setup complete")


if __name__ == "__main__":
    main()
