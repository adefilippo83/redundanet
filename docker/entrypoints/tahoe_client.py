#!/usr/bin/env python3
"""Tahoe-LAFS Client container entrypoint for RedundaNet."""

import os
import subprocess
import sys
import time
from pathlib import Path

from redundanet.utils.logging import setup_logging, get_logger
from redundanet.storage.client import TahoeClient, TahoeClientConfig

# Ports used inside the (shared) tinc network namespace.
TUB_PORT = 3456
WEB_PORT = 4456


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


def get_introducer_furl() -> str | None:
    """Get introducer FURL from environment or the shared manifest volume."""
    logger = get_logger()

    furl = os.environ.get("REDUNDANET_INTRODUCER_FURL")
    if furl:
        return furl

    manifest_dir = Path("/var/lib/redundanet/manifest")
    furl_file = manifest_dir / "introducer.furl"

    if furl_file.exists():
        furl = furl_file.read_text().strip()
        if furl:
            logger.info("Found introducer FURL in manifest volume")
            return furl

    manifest_file = manifest_dir / "manifest.yaml"
    if manifest_file.exists():
        import yaml

        with open(manifest_file) as f:
            manifest = yaml.safe_load(f) or {}

        furl = manifest.get("introducer_furl")
        if furl:
            logger.info("Found introducer FURL in manifest.yaml")
            return furl

    logger.warning("No introducer FURL found")
    return None


def main():
    """Set up the client node configuration, then exit.

    Supervisord then starts the actual `tahoe run` process.
    """
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()

    logger.info("Setting up RedundaNet Tahoe Client")

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

    client_dir = Path("/var/lib/tahoe-client")

    # Wait for VPN to be available (skip in test mode)
    if not test_mode:
        if not wait_for_vpn(vpn_ip):
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

    config = TahoeClientConfig(
        nickname=f"{node_name}-client",
        node_dir=client_dir,
        introducer_furl=introducer_furl,
        web_port=WEB_PORT,
        tub_port=TUB_PORT,
        tub_location=f"tcp:{vpn_ip}:{TUB_PORT}",
        shares_needed=shares_needed,
        shares_happy=shares_happy,
        shares_total=shares_total,
    )
    client = TahoeClient(config)

    if not client.is_configured():
        logger.info("Creating new Tahoe client", node=node_name)
        client.create_node()
    else:
        logger.info("Using existing Tahoe client configuration")
        client.update_introducer_furl(introducer_furl)

    logger.info("Tahoe client setup complete")


if __name__ == "__main__":
    main()
