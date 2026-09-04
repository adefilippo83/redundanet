#!/usr/bin/env python3
"""Tahoe-LAFS Client container entrypoint for RedundaNet."""

import os
import subprocess
import sys
import time
from pathlib import Path

from redundanet.utils.logging import setup_logging, get_logger
from redundanet.storage.client import TahoeClient, TahoeClientConfig
from redundanet.storage.introducers import dedupe, introducer_furls_from_manifest

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


def get_introducer_furls() -> list[str]:
    """Every introducer FURL this client should use, primary first.

    Sources, in precedence order: REDUNDANET_INTRODUCER_FURL (an explicit
    override, used alone); the FURL a local introducer container published to
    the shared manifest volume; then the manifest's top-level introducer_furl
    and each introducer node's own introducer_furl. Servers are learned from
    all of them, so any single introducer can be down.
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

    # The manifest dir may be a plain dir or a full repo clone.
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


def _prepare_sftp(client_dir: Path) -> None:
    """Ensure SFTP host keys and an accounts file exist (idempotent).

    Host keys are generated once and persist on the volume. The accounts file
    starts empty (the SFTP server still starts; nobody can log in until an
    account is added with `redundanet storage sftp adduser`).
    """
    logger = get_logger()
    private = client_dir / "private"
    private.mkdir(parents=True, exist_ok=True)

    privkey = private / "ssh_host_rsa_key"
    if not privkey.exists():
        subprocess.run(
            ["ssh-keygen", "-t", "rsa", "-b", "2048", "-N", "", "-f", str(privkey)],
            check=True,
            capture_output=True,
        )
        logger.info("Generated SFTP host key")

    accounts = private / "sftp_accounts"
    if not accounts.exists():
        accounts.write_text(
            "# RedundaNet SFTP accounts — one per line:\n"
            "#   <username> ssh-rsa <key-blob> <root-directory-cap>\n"
            "# Managed by 'redundanet storage sftp adduser'.\n"
        )
        logger.info("Created empty SFTP accounts file")


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
    sftp_enabled = os.environ.get("REDUNDANET_SFTP_ENABLED", "false").lower() == "true"
    sftp_port = int(os.environ.get("REDUNDANET_SFTP_PORT", "8022"))
    test_mode = os.environ.get("REDUNDANET_TEST_MODE", "false").lower() == "true"

    if not node_name:
        logger.error("REDUNDANET_NODE_NAME environment variable is required")
        sys.exit(1)

    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP environment variable is required")
        sys.exit(1)

    client_dir = Path("/var/lib/tahoe-client")

    # Fresh ext4 volumes (a fly volume or a real disk) contain lost+found, which
    # makes `tahoe create-client` refuse the "non-empty" base directory — the same
    # guard the introducer/storage entrypoints already have.
    lost_found = client_dir / "lost+found"
    if lost_found.is_dir():
        try:
            lost_found.rmdir()
            logger.info("Removed empty lost+found from fresh volume")
        except OSError:
            logger.warning("lost+found is not empty; not touching it")

    # Wait for VPN to be available (skip in test mode)
    if not test_mode:
        if not wait_for_vpn(vpn_ip):
            logger.error("VPN not available, cannot start client")
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

    config = TahoeClientConfig(
        nickname=f"{node_name}-client",
        node_dir=client_dir,
        introducer_furl=introducer_furls[0],
        extra_introducer_furls=introducer_furls[1:],
        web_port=WEB_PORT,
        tub_port=TUB_PORT,
        tub_location=f"tcp:{vpn_ip}:{TUB_PORT}",
        shares_needed=shares_needed,
        shares_happy=shares_happy,
        shares_total=shares_total,
        sftp_enabled=sftp_enabled,
        sftp_port=sftp_port,
    )
    client = TahoeClient(config)

    if not client.is_configured():
        logger.info("Creating new Tahoe client", node=node_name)
        client.create_node()
    else:
        logger.info("Using existing Tahoe client configuration")
        client.update_introducers(introducer_furls)

    # SFTP host keys + accounts go into the node's private/ dir, which only
    # exists AFTER create_node() (tahoe refuses a non-empty base directory).
    if sftp_enabled:
        _prepare_sftp(client_dir)

    logger.info("Tahoe client setup complete", sftp=sftp_enabled)


if __name__ == "__main__":
    main()
