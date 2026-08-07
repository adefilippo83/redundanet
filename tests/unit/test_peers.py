"""Unit tests for manifest-driven Tinc peer host-file synchronization."""

from __future__ import annotations

from pathlib import Path

import pgpy
import pytest
from pgpy.constants import (
    CompressionAlgorithm,
    HashAlgorithm,
    KeyFlags,
    PubKeyAlgorithm,
    SymmetricKeyAlgorithm,
)

from redundanet.vpn.peers import sync_peer_host_files, tinc_name


def _new_key(email: str) -> pgpy.PGPKey:
    key = pgpy.PGPKey.new(PubKeyAlgorithm.RSAEncryptOrSign, 2048)
    uid = pgpy.PGPUID.new("Peer Test", email=email)
    key.add_uid(
        uid,
        usage={KeyFlags.Sign},
        hashes=[HashAlgorithm.SHA256],
        ciphers=[SymmetricKeyAlgorithm.AES256],
        compression=[CompressionAlgorithm.ZLIB],
    )
    return key


@pytest.fixture(scope="module")
def peer_keys() -> dict[str, tuple[str, str]]:
    """Two peer identities: name -> (fingerprint, armored public key)."""
    keys = {}
    for name in ("peer-a", "peer-b"):
        key = _new_key(f"{name}@test.local")
        fingerprint = str(key.fingerprint).replace(" ", "").upper()
        keys[name] = (fingerprint, str(key.pubkey))
    return keys


def make_manifest_env(tmp_path: Path, peer_keys) -> tuple[Path, Path, list[dict]]:
    """Lay out a manifest dir with local gpg keys, and a node list."""
    manifest_dir = tmp_path / "manifest"
    gpg_dir = manifest_dir / "gpg"
    gpg_dir.mkdir(parents=True)
    hosts_dir = tmp_path / "hosts"

    nodes = [{"name": "self-node", "internal_ip": "10.100.0.1"}]
    for i, (name, (fingerprint, armored)) in enumerate(peer_keys.items(), start=2):
        (gpg_dir / f"{fingerprint}.asc").write_text(armored)
        nodes.append(
            {
                "name": name,
                "internal_ip": f"10.100.0.{i}",
                "vpn_ip": f"10.100.0.{i}",
                "gpg_key_id": fingerprint,
                "ports": {"tinc": 655},
            }
        )
    return manifest_dir, hosts_dir, nodes


def no_fetch(_key_id: str) -> None:
    return None


class TestTincName:
    def test_dashes_become_underscores(self):
        assert tinc_name("node-dd2be971") == "node_dd2be971"


class TestSync:
    def test_writes_peer_host_files(self, tmp_path, peer_keys):
        manifest_dir, hosts_dir, nodes = make_manifest_env(tmp_path, peer_keys)
        nodes[1]["is_publicly_accessible"] = True
        nodes[1]["public_ip"] = "203.0.113.1"

        result = sync_peer_host_files(
            nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch
        )

        assert result.changed
        assert sorted(result.written) == ["peer_a", "peer_b"]
        # Only the publicly accessible peer lands in ConnectTo.
        assert result.connect_to == ["peer_a"]

        pub = (hosts_dir / "peer_a").read_text()
        assert "Address = 203.0.113.1" in pub
        assert "BEGIN RSA PUBLIC KEY" in pub
        nat = (hosts_dir / "peer_b").read_text()
        assert "Address" not in nat

    def test_second_run_is_idempotent(self, tmp_path, peer_keys):
        manifest_dir, hosts_dir, nodes = make_manifest_env(tmp_path, peer_keys)
        first = sync_peer_host_files(
            nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch
        )
        second = sync_peer_host_files(
            nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch
        )
        assert first.changed
        assert not second.changed
        assert second.written == [] and second.removed == []

    def test_removed_node_loses_host_file_but_self_kept(self, tmp_path, peer_keys):
        """Revocation: dropping a node from the manifest removes its host file."""
        manifest_dir, hosts_dir, nodes = make_manifest_env(tmp_path, peer_keys)
        hosts_dir.mkdir(parents=True)
        (hosts_dir / "self_node").write_text("Subnet = 10.100.0.1/32\n")

        sync_peer_host_files(nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch)
        assert (hosts_dir / "peer_b").exists()

        without_b = [n for n in nodes if n["name"] != "peer-b"]
        result = sync_peer_host_files(
            without_b, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch
        )

        assert result.changed
        assert result.removed == ["peer_b"]
        assert not (hosts_dir / "peer_b").exists()
        assert (hosts_dir / "peer_a").exists()
        assert (hosts_dir / "self_node").exists()  # never remove our own file

    def test_unresolvable_peer_keeps_existing_host_file(self, tmp_path, peer_keys):
        """A keyserver outage must not sever an authorized, already-known peer."""
        manifest_dir, hosts_dir, nodes = make_manifest_env(tmp_path, peer_keys)
        sync_peer_host_files(nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch)

        # Simulate the local key becoming unavailable (and no keyserver).
        fingerprint_a = peer_keys["peer-a"][0]
        (manifest_dir / "gpg" / f"{fingerprint_a}.asc").unlink()

        result = sync_peer_host_files(
            nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch
        )
        assert not result.changed
        assert "peer_a" in result.skipped
        assert (hosts_dir / "peer_a").exists()

    def test_mismatched_local_key_is_rejected(self, tmp_path, peer_keys):
        """A local key file whose fingerprint doesn't match its name is ignored."""
        manifest_dir, hosts_dir, nodes = make_manifest_env(tmp_path, peer_keys)
        fpr_a, _ = peer_keys["peer-a"]
        _, armored_b = peer_keys["peer-b"]
        # Overwrite peer-a's key file with peer-b's key (identity substitution).
        (manifest_dir / "gpg" / f"{fpr_a}.asc").write_text(armored_b)

        result = sync_peer_host_files(
            nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch
        )
        assert "peer_a" in result.skipped
        assert not (hosts_dir / "peer_a").exists()
        assert (hosts_dir / "peer_b").exists()

    def test_skipped_public_peer_not_in_connect_to(self, tmp_path, peer_keys):
        manifest_dir, hosts_dir, nodes = make_manifest_env(tmp_path, peer_keys)
        nodes[1]["is_publicly_accessible"] = True
        nodes[1]["public_ip"] = "203.0.113.1"
        fingerprint_a = peer_keys["peer-a"][0]
        (manifest_dir / "gpg" / f"{fingerprint_a}.asc").unlink()

        result = sync_peer_host_files(
            nodes, "self-node", hosts_dir, manifest_dir, fetch_key=no_fetch
        )
        # peer-a is public but has no host file -> must not be advertised.
        assert result.connect_to == []
