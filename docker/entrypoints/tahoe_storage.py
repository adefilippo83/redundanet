#!/usr/bin/env python3
"""Tahoe-LAFS Storage container entrypoint for RedundaNet."""

import os
import subprocess
import sys
import time
from pathlib import Path

from redundanet.utils.logging import setup_logging, get_logger
from redundanet.storage.storage import TahoeStorage


def wait_for_vpn(timeout: int = 300) -> bool:
    """Wait for VPN interface to be available."""
    logger = get_logger()
    start_time = time.time()

    while time.time() - start_time < timeout:
        try:
            result = subprocess.run(
                ["ip", "link", "show", "tinc0"],
                capture_output=True,
                text=True,
            )
            if result.returncode == 0:
                logger.info("VPN interface is available")
                return True
        except Exception:
            pass

        logger.debug("Waiting for VPN interface...")
        time.sleep(5)

    logger.error("Timeout waiting for VPN interface")
    return False


def get_introducer_furl() -> str | None:
    """Get introducer FURL from manifest or environment."""
    logger = get_logger()

    # Check environment variable first
    furl = os.environ.get("REDUNDANET_INTRODUCER_FURL")
    if furl:
        return furl

    # Check manifest directory
    manifest_dir = Path("/var/lib/redundanet/manifest")
    furl_file = manifest_dir / "introducer.furl"

    if furl_file.exists():
        furl = furl_file.read_text().strip()
        if furl:
            logger.info("Found introducer FURL in manifest")
            return furl

    # Check manifest.yaml
    manifest_file = manifest_dir / "manifest.yaml"
    if manifest_file.exists():
        import yaml
        with open(manifest_file) as f:
            manifest = yaml.safe_load(f)

        furl = manifest.get("tahoe", {}).get("introducer_furl")
        if furl:
            logger.info("Found introducer FURL in manifest.yaml")
            return furl

    logger.warning("No introducer FURL found")
    return None


def main():
    """Main entrypoint for Tahoe Storage container.

    This script sets up the storage configuration, then exits.
    Supervisord will then start the actual tahoe process.
    """
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()

    logger.info("Setting up RedundaNet Tahoe Storage")

    # Get configuration from environment
    node_name = os.environ.get("REDUNDANET_NODE_NAME")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP")
    reserved_space = os.environ.get("REDUNDANET_RESERVED_SPACE", "50G")
    test_mode = os.environ.get("REDUNDANET_TEST_MODE", "false").lower() == "true"

    if not node_name:
        logger.error("REDUNDANET_NODE_NAME environment variable is required")
        sys.exit(1)

    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP environment variable is required")
        sys.exit(1)

    # Paths
    storage_dir = Path("/var/lib/tahoe-storage")
    storage_data_dir = Path("/data/storage")

    # Wait for VPN to be available (skip in test mode)
    if not test_mode:
        if not wait_for_vpn():
            logger.error("VPN not available, cannot start storage node")
            sys.exit(1)
    else:
        logger.info("Test mode: skipping VPN wait")

    # Get introducer FURL (retry with backoff)
    introducer_furl = None
    for attempt in range(30):
        introducer_furl = get_introducer_furl()
        if introducer_furl:
            break
        logger.info("Waiting for introducer FURL...", attempt=attempt + 1)
        time.sleep(10)

    if not introducer_furl:
        logger.error("Could not obtain introducer FURL")
        sys.exit(1)

    # Initialize storage node
    storage = TahoeStorage(base_dir=storage_dir)

    # Check if storage is already configured
    if not storage.is_configured():
        logger.info("Creating new Tahoe storage node", node=node_name)
        storage.create(
            nickname=f"{node_name}-storage",
            introducer_furl=introducer_furl,
            port=3457,
            location=f"tcp:{vpn_ip}:3457",
            reserved_space=reserved_space,
            storage_dir=storage_data_dir,
        )
    else:
        logger.info("Using existing Tahoe storage configuration")
        # Update introducer FURL if it changed
        storage.update_introducer(introducer_furl)

    logger.info("Tahoe storage setup complete")


if __name__ == "__main__":
    main()
