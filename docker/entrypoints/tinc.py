#!/usr/bin/env python3
"""Tinc VPN container entrypoint for RedundaNet.

GPG-only auth: a node reuses its own GPG (RSA) key as its Tinc transport key, and
builds each peer's Tinc host file from that peer's GPG public key (resolved by
``gpg_key_id`` from a local file or the keyservers). No separate Tinc keypair is
generated, and no key material beyond the published GPG keys is needed.

Peer host files are kept up to date afterwards by the manifest-sync sidecar
process (manifest_sync.py, run by supervisord next to this entrypoint).
"""

import os
import sys
from pathlib import Path

from redundanet.core.deployment import git_sync
from redundanet.utils.logging import get_logger, setup_logging
from redundanet.vpn.gpg_tinc import gpg_public_to_tinc_pub, gpg_secret_to_tinc_priv
from redundanet.vpn.peers import render_host_file, sync_peer_host_files, tinc_name
from redundanet.vpn.tinc import TincConfig, TincManager

GPG_SECRET_PATH = Path("/run/secrets/gpg_private_key")
MANIFEST_DIR = Path("/var/lib/redundanet/manifest")
TINC_CONFIG_DIR = Path("/etc/tinc/redundanet")


def sync_manifest(manifest_repo: str, manifest_branch: str, manifest_dir: Path) -> bool:
    """Sync manifest from a Git repository."""
    logger = get_logger()
    if not manifest_repo:
        logger.warning("No manifest repository configured")
        return False
    result = git_sync(manifest_repo, manifest_branch, manifest_dir)
    if result.success:
        logger.info("Manifest synced", repo=manifest_repo, branch=manifest_branch)
        return True
    logger.error("Failed to sync manifest", error=result.stderr.strip())
    return False


def main() -> None:
    setup_logging(level=os.environ.get("REDUNDANET_LOG_LEVEL", "INFO"))
    logger = get_logger()
    logger.info("Starting RedundaNet Tinc VPN container (GPG-only auth)")

    node_name = os.environ.get("REDUNDANET_NODE_NAME")
    vpn_ip = os.environ.get("REDUNDANET_INTERNAL_VPN_IP")
    public_ip = os.environ.get("REDUNDANET_PUBLIC_IP", "auto")
    manifest_repo = os.environ.get("REDUNDANET_MANIFEST_REPO", "")
    manifest_branch = os.environ.get("REDUNDANET_MANIFEST_BRANCH", "main")
    debug = os.environ.get("REDUNDANET_DEBUG", "false").lower() == "true"

    if not node_name:
        logger.error("REDUNDANET_NODE_NAME is required")
        sys.exit(1)
    if not vpn_ip:
        logger.error("REDUNDANET_INTERNAL_VPN_IP is required")
        sys.exit(1)

    tinc_node_name = tinc_name(node_name)

    if manifest_repo:
        sync_manifest(manifest_repo, manifest_branch, MANIFEST_DIR)

    # The node's GPG private key (mounted secret) IS the Tinc key material.
    if not GPG_SECRET_PATH.exists():
        logger.error("GPG private key not found at /run/secrets/gpg_private_key")
        sys.exit(1)
    gpg_secret = GPG_SECRET_PATH.read_text()
    try:
        priv_pem = gpg_secret_to_tinc_priv(gpg_secret)
        pub_pem = gpg_public_to_tinc_pub(gpg_secret)
    except Exception as e:
        logger.error("Failed to derive Tinc key from GPG key", error=str(e))
        sys.exit(1)

    # Detect public IP if requested.
    if public_ip == "auto":
        try:
            import httpx

            public_ip = httpx.get("https://api.ipify.org", timeout=10).text.strip()
            logger.info("Detected public IP", ip=public_ip)
        except Exception:
            public_ip = ""
            logger.warning("Could not detect public IP")

    # Load the manifest (peers + ports).
    nodes: list[dict] = []
    manifest_file = MANIFEST_DIR / "manifest.yaml"
    if manifest_file.exists():
        import yaml

        manifest = yaml.safe_load(manifest_file.read_text()) or {}
        nodes = manifest.get("nodes", [])

    self_entry = next((n for n in nodes if n.get("name") == node_name), {})
    self_port = int(self_entry.get("ports", {}).get("tinc", 655))
    self_public = public_ip or None

    config = TincConfig(
        network_name="redundanet",
        node_name=tinc_node_name,
        vpn_ip=vpn_ip,
        public_ip=self_public,
        port=self_port,
        connect_to=[],
        config_dir=TINC_CONFIG_DIR.parent,  # network_dir appends /redundanet
    )
    tinc = TincManager(config=config)
    network_dir = config.network_dir
    hosts_dir = config.hosts_dir
    hosts_dir.mkdir(parents=True, exist_ok=True)

    # Write our own Tinc key + host file (from the GPG key).
    priv_path = network_dir / "rsa_key.priv"
    priv_path.write_text(priv_pem)
    priv_path.chmod(0o600)
    (hosts_dir / tinc_node_name).write_text(
        render_host_file(vpn_ip, self_public, self_port, pub_pem)
    )
    logger.info("Derived Tinc key from GPG key", node=tinc_node_name)

    # Build each peer's host file from its GPG public key (and drop host files
    # of nodes that are no longer in the manifest).
    peers = sync_peer_host_files(nodes, node_name, hosts_dir, MANIFEST_DIR)

    # Write tinc.conf / tinc-up / tinc-down. setup() sees the existing key and
    # host files and won't overwrite them; with no peers passed it won't touch
    # the peer host files just written.
    config.connect_to = peers.connect_to
    tinc.setup()

    logger.info("Tinc configuration complete, starting tincd", connect_to=peers.connect_to)
    tincd_args = ["tincd", "-n", "redundanet", "-D"]
    if debug:
        tincd_args.append("-d5")
    # Replace this process with tincd so supervisord manages it directly.
    os.execvp("tincd", tincd_args)  # noqa: S606


if __name__ == "__main__":
    main()
