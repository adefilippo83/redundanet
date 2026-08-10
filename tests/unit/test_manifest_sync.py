"""Unit tests for the tinc container's manifest-sync sidecar wiring."""

from __future__ import annotations

import sys
from pathlib import Path

import pgpy
import pytest
import yaml
from pgpy.constants import (
    CompressionAlgorithm,
    HashAlgorithm,
    KeyFlags,
    PubKeyAlgorithm,
    SymmetricKeyAlgorithm,
)

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import manifest_sync  # noqa: E402


@pytest.fixture(scope="module")
def peer_key() -> tuple[str, str]:
    key = pgpy.PGPKey.new(PubKeyAlgorithm.RSAEncryptOrSign, 2048)
    uid = pgpy.PGPUID.new("Sync Test", email="sync@test.local")
    key.add_uid(
        uid,
        usage={KeyFlags.Sign},
        hashes=[HashAlgorithm.SHA256],
        ciphers=[SymmetricKeyAlgorithm.AES256],
        compression=[CompressionAlgorithm.ZLIB],
    )
    return str(key.fingerprint).replace(" ", "").upper(), str(key.pubkey)


def write_env(tmp_path: Path, peer_key, include_peer: bool) -> tuple[Path, Path]:
    """Manifest dir + tinc config dir for a two-node network."""
    fingerprint, armored = peer_key
    manifest_dir = tmp_path / "manifest"
    (manifest_dir / "gpg").mkdir(parents=True, exist_ok=True)
    (manifest_dir / "gpg" / f"{fingerprint}.asc").write_text(armored)

    nodes = [{"name": "self-node", "internal_ip": "10.100.0.1", "ports": {"tinc": 655}}]
    if include_peer:
        nodes.append(
            {
                "name": "peer-a",
                "internal_ip": "10.100.0.2",
                "vpn_ip": "10.100.0.2",
                "gpg_key_id": fingerprint,
                "is_publicly_accessible": True,
                "public_ip": "203.0.113.9",
            }
        )
    (manifest_dir / "manifest.yaml").write_text(yaml.dump({"nodes": nodes}))

    config_dir = tmp_path / "tinc" / "redundanet"
    (config_dir / "hosts").mkdir(parents=True, exist_ok=True)
    return manifest_dir, config_dir


class TestRunOnce:
    def test_new_peer_triggers_conf_rewrite_and_reload(self, tmp_path, monkeypatch, peer_key):
        monkeypatch.setenv("REDUNDANET_INTERNAL_VPN_IP", "10.100.0.1")
        manifest_dir, config_dir = write_env(tmp_path, peer_key, include_peer=True)
        reloads: list[bool] = []

        changed = manifest_sync.run_once(
            "self-node",
            repo="",  # static manifest already on disk
            branch="main",
            manifest_dir=manifest_dir,
            config_dir=config_dir,
            reload_tincd=lambda: reloads.append(True) or True,
        )

        assert changed
        assert reloads == [True]
        assert (config_dir / "hosts" / "peer_a").exists()
        conf = (config_dir / "tinc.conf").read_text()
        assert "ConnectTo = peer_a" in conf

    def test_no_change_means_no_reload(self, tmp_path, monkeypatch, peer_key):
        monkeypatch.setenv("REDUNDANET_INTERNAL_VPN_IP", "10.100.0.1")
        manifest_dir, config_dir = write_env(tmp_path, peer_key, include_peer=True)
        manifest_sync.run_once(
            "self-node", "", "main", manifest_dir, config_dir, reload_tincd=lambda: True
        )

        reloads: list[bool] = []
        changed = manifest_sync.run_once(
            "self-node",
            "",
            "main",
            manifest_dir,
            config_dir,
            reload_tincd=lambda: reloads.append(True) or True,
        )
        assert not changed
        assert reloads == []

    def test_revoked_peer_is_dropped_and_tincd_reloaded(self, tmp_path, monkeypatch, peer_key):
        monkeypatch.setenv("REDUNDANET_INTERNAL_VPN_IP", "10.100.0.1")
        manifest_dir, config_dir = write_env(tmp_path, peer_key, include_peer=True)
        manifest_sync.run_once(
            "self-node", "", "main", manifest_dir, config_dir, reload_tincd=lambda: True
        )
        assert (config_dir / "hosts" / "peer_a").exists()

        # Rewrite the manifest without the peer (revocation).
        write_env(tmp_path, peer_key, include_peer=False)
        reloads: list[bool] = []
        changed = manifest_sync.run_once(
            "self-node",
            "",
            "main",
            manifest_dir,
            config_dir,
            reload_tincd=lambda: reloads.append(True) or True,
        )

        assert changed
        assert reloads == [True]
        assert not (config_dir / "hosts" / "peer_a").exists()
        assert (
            "ConnectTo" not in (config_dir / "tinc.conf").read_text().replace("# ", "")
            or "ConnectTo = peer_a" not in (config_dir / "tinc.conf").read_text()
        )

    def test_repo_clone_layout_is_found(self, tmp_path, monkeypatch, peer_key):
        """When the manifest dir is a repo clone, the manifest lives under
        manifests/ — the sync must still find it (the bug that left the live
        hub blind to new peers)."""
        monkeypatch.setenv("REDUNDANET_INTERNAL_VPN_IP", "10.100.0.1")
        manifest_dir, config_dir = write_env(tmp_path, peer_key, include_peer=True)
        # Move the manifest into the repo-clone layout.
        (manifest_dir / "manifests").mkdir()
        (manifest_dir / "manifest.yaml").rename(manifest_dir / "manifests" / "manifest.yaml")

        changed = manifest_sync.run_once(
            "self-node", "", "main", manifest_dir, config_dir, reload_tincd=lambda: True
        )
        assert changed
        assert (config_dir / "hosts" / "peer_a").exists()

    def test_missing_manifest_is_a_noop(self, tmp_path):
        changed = manifest_sync.run_once(
            "self-node",
            "",
            "main",
            tmp_path / "nope",
            tmp_path / "tinc" / "redundanet",
            reload_tincd=lambda: True,
        )
        assert not changed
