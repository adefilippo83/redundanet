#!/usr/bin/env python3
"""Tahoe-LAFS Client container entrypoint for RedundaNet."""

import os
import subprocess
import sys
import time
from pathlib import Path

from redundanet.utils.logging import setup_logging, get_logger
from redundanet.storage.client import TahoeClient


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
    """Main entrypoint for Tahoe Client container.

    This script sets up the client configuration, then exits.
    Supervisord will then start the actual tahoe process.
    """
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()

    logger.info("Setting up RedundaNet Tahoe Client")

    # Get configuration from environment
    node_name = os.environ.get("REDUNDANET_NODE_NAME")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP")
    shares_needed = int(os.environ.get("REDUNDANET_SHARES_NEEDED", "3"))
    shares_happy = int(os.environ.get("REDUNDANET_SHARES_HAPPY", "7"))
    shares_total = int(os.environ.get("REDUNDANET_SHARES_TOTAL", "10"))
    test_mode = os.environ.get("REDUNDANET_TEST_MODE", "false").lower() == "true"

    if not node_name:
        logger.error("REDUNDANET_NODE_NAME environment variable is required")
        sys.exit(1)

    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP environment variable is required")
        sys.exit(1)

    # Paths
    client_dir = Path("/var/lib/tahoe-client")
    mount_point = Path("/mnt/redundanet")

    # Wait for VPN to be available (skip in test mode)
    if not test_mode:
        if not wait_for_vpn():
            logger.error("VPN not available, cannot start client")
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

    # Initialize client
    client = TahoeClient(base_dir=client_dir)

    # Check if client is already configured
    if not client.is_configured():
        logger.info("Creating new Tahoe client", node=node_name)
        client.create(
            nickname=f"{node_name}-client",
            introducer_furl=introducer_furl,
            shares_needed=shares_needed,
            shares_happy=shares_happy,
            shares_total=shares_total,
        )
    else:
        logger.info("Using existing Tahoe client configuration")
        # Update introducer FURL if it changed
        client.update_introducer(introducer_furl)

    logger.info("Tahoe client setup complete")


if __name__ == "__main__":
    main()
