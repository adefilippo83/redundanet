#!/usr/bin/env python3
"""Tinc VPN container entrypoint for RedundaNet."""

import os
import subprocess
import sys
from pathlib import Path

from redundanet.core.config import AppSettings
from redundanet.utils.logging import setup_logging, get_logger
from redundanet.vpn.tinc import TincConfig, TincManager
from redundanet.auth.gpg import GPGManager


def wait_for_gpg_key() -> str | None:
    """Wait for GPG private key to be available."""
    key_path = Path("/run/secrets/gpg_private_key")
    if key_path.exists():
        return str(key_path)
    return None


def sync_manifest(manifest_repo: str, manifest_branch: str, manifest_dir: Path) -> bool:
    """Sync manifest from Git repository."""
    logger = get_logger()

    if not manifest_repo:
        logger.warning("No manifest repository configured")
        return False

    try:
        if (manifest_dir / ".git").exists():
            # Pull latest changes
            subprocess.run(
                ["git", "-C", str(manifest_dir), "fetch", "origin"],
                check=True,
                capture_output=True,
            )
            subprocess.run(
                ["git", "-C", str(manifest_dir), "reset", "--hard", f"origin/{manifest_branch}"],
                check=True,
                capture_output=True,
            )
        else:
            # Clone repository
            manifest_dir.mkdir(parents=True, exist_ok=True)
            subprocess.run(
                ["git", "clone", "-b", manifest_branch, manifest_repo, str(manifest_dir)],
                check=True,
                capture_output=True,
            )

        logger.info("Manifest synced successfully", repo=manifest_repo, branch=manifest_branch)
        return True
    except subprocess.CalledProcessError as e:
        logger.error("Failed to sync manifest", error=str(e))
        return False


def main():
    """Main entrypoint for Tinc container."""
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()

    logger.info("Starting RedundaNet Tinc VPN container")

    # Get configuration from environment
    node_name = os.environ.get("REDUNDANET_NODE_NAME")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP")
    public_ip = os.environ.get("REDUNDANET_PUBLIC_IP", "auto")
    gpg_key_id = os.environ.get("REDUNDANET_GPG_KEY_ID")
    manifest_repo = os.environ.get("REDUNDANET_MANIFEST_REPO", "")
    manifest_branch = os.environ.get("REDUNDANET_MANIFEST_BRANCH", "main")
    debug = os.environ.get("REDUNDANET_DEBUG", "false").lower() == "true"

    if not node_name:
        logger.error("REDUNDANET_NODE_NAME environment variable is required")
        sys.exit(1)

    # Sanitize node name for tinc (only alphanumeric and underscore allowed)
    tinc_node_name = node_name.replace("-", "_")

    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP environment variable is required")
        sys.exit(1)

    # Paths
    tinc_config_dir = Path("/etc/tinc/redundanet")
    manifest_dir = Path("/var/lib/redundanet/manifest")

    # Sync manifest
    if manifest_repo:
        sync_manifest(manifest_repo, manifest_branch, manifest_dir)

    # Initialize GPG if key provided
    gpg_key_path = wait_for_gpg_key()
    if gpg_key_path and gpg_key_id:
        logger.info("Importing GPG key", key_id=gpg_key_id)
        gpg = GPGManager()
        try:
            key_data = Path(gpg_key_path).read_text()
            gpg.import_key(key_data)
        except Exception as e:
            logger.warning("Failed to import GPG key", error=str(e))

    # Determine public IP if set to auto
    if public_ip == "auto":
        try:
            import httpx

            response = httpx.get("https://api.ipify.org", timeout=10)
            public_ip = response.text.strip()
            logger.info("Detected public IP", ip=public_ip)
        except Exception:
            public_ip = ""
            logger.warning("Could not detect public IP")

    # Load peer list from manifest
    connect_to = []
    manifest_file = manifest_dir / "manifest.yaml"
    peers = []
    if manifest_file.exists():
        import yaml

        with open(manifest_file) as f:
            manifest = yaml.safe_load(f)

        for node in manifest.get("nodes", []):
            peer_name = node.get("name")
            if peer_name and peer_name != node_name:
                # Sanitize peer names for tinc compatibility
                connect_to.append(peer_name.replace("-", "_"))

    # Initialize Tinc config and manager
    logger.info("Configuring Tinc VPN", node=tinc_node_name, vpn_ip=vpn_ip)

    tinc_config = TincConfig(
        network_name="redundanet",
        node_name=tinc_node_name,
        vpn_ip=vpn_ip,
        public_ip=public_ip if public_ip else None,
        connect_to=connect_to,
        config_dir=tinc_config_dir.parent,  # /etc/tinc (network_dir adds /redundanet)
    )
    tinc = TincManager(config=tinc_config)

    # Generate keys if needed
    private_key_path = tinc_config.network_dir / "rsa_key.priv"
    if not private_key_path.exists():
        logger.info("Generating new Tinc keypair")
        tinc.generate_keys()

    # Run full setup (writes conf, scripts, host files)
    tinc.setup()

    logger.info("Tinc configuration complete, starting tincd")

    # Build tincd command
    tincd_args = ["tincd", "-n", "redundanet", "-D"]
    if debug:
        tincd_args.append("-d5")

    # Run tincd directly in foreground (replaces this process)
    os.execvp("tincd", tincd_args)


if __name__ == "__main__":
    main()
