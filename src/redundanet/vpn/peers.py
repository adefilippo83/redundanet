"""Build and refresh Tinc peer host files from the network manifest.

Each manifest node's Tinc identity is its GPG key (see
:mod:`redundanet.vpn.gpg_tinc`): the peer's public key is resolved by
``gpg_key_id`` — from a local ``<manifest>/gpg/<id>.asc`` file first, then the
public keyservers — verified against the declared id, converted to PKCS#1 PEM,
and written into a Tinc host file.

Used both by the tinc container entrypoint (initial configuration) and by the
periodic manifest-sync process (docker/entrypoints/manifest_sync.py), which is
what makes joins and revocations propagate to running nodes: a node added to
the manifest gains a host file, a node removed from it loses its host file.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from redundanet.auth.keyserver import armored_key_matches_id
from redundanet.utils.logging import get_logger
from redundanet.vpn.gpg_tinc import gpg_public_to_tinc_pub

logger = get_logger(__name__)

# A fetcher takes a gpg key id and returns the armored public key, or None.
KeyFetcher = Callable[[str], "str | None"]


@dataclass
class PeerSync:
    """Result of refreshing the peer host files."""

    connect_to: list[str] = field(default_factory=list)
    changed: bool = False
    written: list[str] = field(default_factory=list)
    removed: list[str] = field(default_factory=list)
    skipped: list[str] = field(default_factory=list)


def tinc_name(node_name: str) -> str:
    """Tinc identifiers may not contain '-'; manifest names use it freely."""
    return node_name.replace("-", "_")


def render_host_file(vpn_ip: str, public_ip: str | None, port: int, pubkey_pem: str) -> str:
    """Render a Tinc host file (Subnet/Address/Port + the PEM public key)."""
    lines = [f"Subnet = {vpn_ip}/32"]
    if public_ip:
        lines.append(f"Address = {public_ip}")
    lines.append(f"Port = {port}")
    lines.append("")
    lines.append(pubkey_pem.strip())
    lines.append("")
    return "\n".join(lines)


def _keyserver_fetch(gpg_key_id: str) -> str | None:
    from redundanet.auth.gpg import GPGManager
    from redundanet.auth.keyserver import KeyServerClient

    with KeyServerClient(GPGManager()) as client:
        return client.fetch_key(gpg_key_id)


def resolve_peer_pubkey(
    gpg_key_id: str,
    manifest_dir: Path,
    fetch_key: KeyFetcher | None = None,
) -> str | None:
    """Get a peer's armored GPG public key: local file first, then keyservers.

    Whatever the source, the key is only accepted if its fingerprint matches
    the manifest's gpg_key_id — a mismatched key would let an attacker hijack
    the peer's Tinc identity.
    """
    # The manifest dir may be a plain dir (gpg/) or a repo clone (manifests/gpg/).
    for local in (
        manifest_dir / "gpg" / f"{gpg_key_id}.asc",
        manifest_dir / "manifests" / "gpg" / f"{gpg_key_id}.asc",
    ):
        if not local.exists():
            continue
        armored = local.read_text()
        if armored_key_matches_id(armored, gpg_key_id):
            logger.debug("Using local GPG public key", gpg_key_id=gpg_key_id)
            return armored
        logger.warning(
            "Local GPG key file does not match its declared key id; ignoring it",
            gpg_key_id=gpg_key_id,
            path=str(local),
        )

    fetch = fetch_key or _keyserver_fetch
    try:
        fetched = fetch(gpg_key_id)
    except Exception as e:  # network/gpg errors should not crash the node
        logger.warning("Key fetch failed", gpg_key_id=gpg_key_id, error=str(e))
        return None
    if fetched is not None and not armored_key_matches_id(fetched, gpg_key_id):
        logger.warning(
            "Fetched key does not match the declared key id; discarding it",
            gpg_key_id=gpg_key_id,
        )
        return None
    return fetched


def sync_peer_host_files(
    nodes: list[dict[str, Any]],
    node_name: str,
    hosts_dir: Path,
    manifest_dir: Path,
    fetch_key: KeyFetcher | None = None,
) -> PeerSync:
    """Bring the Tinc hosts directory in line with the manifest's node list.

    - Writes/updates a host file per resolvable peer.
    - Leaves a peer's existing host file alone when its key can't currently be
      resolved (a keyserver outage must not sever an authorized peer).
    - Deletes host files of nodes that are no longer in the manifest at all
      (revocation), never the local node's own file.

    Returns the ``ConnectTo`` list (publicly reachable peers) and whether
    anything on disk changed.
    """
    result = PeerSync()
    hosts_dir.mkdir(parents=True, exist_ok=True)
    self_tinc = tinc_name(node_name)

    # Every manifest node (self included) keeps its host file, even if its key
    # is unresolvable this round; only nodes gone from the manifest are removed.
    keep = {self_tinc}

    for node in nodes:
        peer_name = node.get("name")
        if not peer_name:
            continue
        peer_tinc = tinc_name(peer_name)
        keep.add(peer_tinc)
        if peer_name == node_name:
            continue

        peer_public = node.get("public_ip") if node.get("is_publicly_accessible") else None
        if peer_public:
            result.connect_to.append(peer_tinc)

        peer_gpg = node.get("gpg_key_id")
        if not peer_gpg:
            logger.warning("Peer has no gpg_key_id, skipping", peer=peer_name)
            result.skipped.append(peer_tinc)
            continue
        armored = resolve_peer_pubkey(str(peer_gpg), manifest_dir, fetch_key=fetch_key)
        if not armored:
            logger.warning("No GPG key for peer, skipping", peer=peer_name, gpg_key_id=peer_gpg)
            result.skipped.append(peer_tinc)
            continue
        try:
            peer_pub = gpg_public_to_tinc_pub(armored)
        except Exception as e:
            logger.warning("Failed to convert peer GPG key", peer=peer_name, error=str(e))
            result.skipped.append(peer_tinc)
            continue

        peer_vpn = node.get("vpn_ip") or node.get("internal_ip")
        if not peer_vpn:
            logger.warning("Peer has no VPN/internal IP, skipping", peer=peer_name)
            result.skipped.append(peer_tinc)
            continue
        peer_port = int((node.get("ports", {}) or {}).get("tinc", 655))
        content = render_host_file(str(peer_vpn), peer_public, peer_port, peer_pub)

        host_path = hosts_dir / peer_tinc
        if not host_path.exists() or host_path.read_text() != content:
            host_path.write_text(content)
            result.changed = True
            result.written.append(peer_tinc)
            logger.info("Wrote peer host file", peer=peer_tinc, reachable=bool(peer_public))

    # A skipped peer with no ConnectTo host file would break tincd startup;
    # only advertise peers whose host file actually exists.
    result.connect_to = [p for p in result.connect_to if (hosts_dir / p).exists()]

    for host_file in hosts_dir.iterdir():
        if host_file.is_file() and host_file.name not in keep:
            host_file.unlink()
            result.changed = True
            result.removed.append(host_file.name)
            logger.info("Removed host file for node no longer in manifest", peer=host_file.name)

    return result
