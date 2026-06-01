#!/usr/bin/env python3
"""Tinc VPN container entrypoint for RedundaNet.

GPG-only auth: a node reuses its own GPG (RSA) key as its Tinc transport key, and
builds each peer's Tinc host file from that peer's GPG public key (resolved by
``gpg_key_id`` from a local file or the keyservers). No separate Tinc keypair is
generated, and no key material beyond the published GPG keys is needed.
"""

import os
import subprocess
import sys
from pathlib import Path

from redundanet.auth.gpg import GPGManager
from redundanet.auth.keyserver import KeyServerClient
from redundanet.utils.logging import get_logger, setup_logging
from redundanet.vpn.gpg_tinc import gpg_public_to_tinc_pub, gpg_secret_to_tinc_priv
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
    try:
        if (manifest_dir / ".git").exists():
            subprocess.run(
                ["git", "-C", str(manifest_dir), "fetch", "origin"], check=True, capture_output=True
            )
            subprocess.run(
                ["git", "-C", str(manifest_dir), "reset", "--hard", f"origin/{manifest_branch}"],
                check=True,
                capture_output=True,
            )
        else:
            manifest_dir.mkdir(parents=True, exist_ok=True)
            subprocess.run(
                ["git", "clone", "-b", manifest_branch, manifest_repo, str(manifest_dir)],
                check=True,
                capture_output=True,
            )
        logger.info("Manifest synced", repo=manifest_repo, branch=manifest_branch)
        return True
    except subprocess.CalledProcessError as e:
        logger.error("Failed to sync manifest", error=str(e))
        return False


def _resolve_peer_pubkey(gpg_key_id: str) -> str | None:
    """Get a peer's armored GPG public key: local file first, then keyservers."""
    logger = get_logger()
    local = MANIFEST_DIR / "gpg" / f"{gpg_key_id}.asc"
    if local.exists():
        logger.info("Using local GPG public key", gpg_key_id=gpg_key_id)
        return local.read_text()
    try:
        armored = KeyServerClient(GPGManager()).fetch_key(gpg_key_id)
    except Exception as e:  # network/gpg errors should not crash the node
        logger.warning("Keyserver fetch failed", gpg_key_id=gpg_key_id, error=str(e))
        return None
    return armored


def _host_file(vpn_ip: str, public_ip: str | None, port: int, pubkey_pem: str) -> str:
    """Render a Tinc host file (Subnet/Address/Port + the PEM public key)."""
    lines = [f"Subnet = {vpn_ip}/32"]
    if public_ip:
        lines.append(f"Address = {public_ip}")
    lines.append(f"Port = {port}")
    lines.append("")
    lines.append(pubkey_pem.strip())
    lines.append("")
    return "\n".join(lines)


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

    tinc_node_name = node_name.replace("-", "_")

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
        _host_file(vpn_ip, self_public, self_port, pub_pem)
    )
    logger.info("Derived Tinc key from GPG key", node=tinc_node_name)

    # Build each peer's host file from its GPG public key.
    connect_to: list[str] = []
    for node in nodes:
        peer_name = node.get("name")
        if not peer_name or peer_name == node_name:
            continue
        peer_gpg = node.get("gpg_key_id")
        if not peer_gpg:
            logger.warning("Peer has no gpg_key_id, skipping", peer=peer_name)
            continue
        armored = _resolve_peer_pubkey(peer_gpg)
        if not armored:
            logger.warning("No GPG key for peer, skipping", peer=peer_name, gpg_key_id=peer_gpg)
            continue
        try:
            peer_pub = gpg_public_to_tinc_pub(armored)
        except Exception as e:
            logger.warning("Failed to convert peer GPG key", peer=peer_name, error=str(e))
            continue

        peer_tinc_name = peer_name.replace("-", "_")
        peer_vpn = node.get("vpn_ip") or node.get("internal_ip")
        peer_public = node.get("public_ip") if node.get("is_publicly_accessible") else None
        peer_port = int(node.get("ports", {}).get("tinc", 655))
        (hosts_dir / peer_tinc_name).write_text(
            _host_file(peer_vpn, peer_public, peer_port, peer_pub)
        )
        if peer_public:
            connect_to.append(peer_tinc_name)
        logger.info("Wrote peer host file", peer=peer_tinc_name, reachable=bool(peer_public))

    # Write tinc.conf / tinc-up / tinc-down. setup() sees the existing key and host
    # file and won't regenerate or overwrite them; with no peers passed it won't
    # touch the peer host files we just wrote.
    config.connect_to = connect_to
    tinc.setup()

    logger.info("Tinc configuration complete, starting tincd", connect_to=connect_to)
    tincd_args = ["tincd", "-n", "redundanet", "-D"]
    if debug:
        tincd_args.append("-d5")
    os.execvp("tincd", tincd_args)


if __name__ == "__main__":
    main()
