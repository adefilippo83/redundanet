"""Unit tests for Tinc configuration generation."""

from pathlib import Path

import pytest

from redundanet.core.config import NodeConfig
from redundanet.core.exceptions import VPNError
from redundanet.vpn.tinc import TincConfig, TincManager


def make_config(tmp_path: Path, **kw) -> TincConfig:
    defaults = {
        "config_dir": tmp_path,
        "network_name": "testnet",
        "node_name": "test-node",
        "vpn_ip": "10.100.0.1",
    }
    defaults.update(kw)
    return TincConfig(**defaults)


class TestTincConfigGeneration:
    def test_write_tinc_conf(self, tmp_path: Path):
        config = make_config(tmp_path, public_ip="1.2.3.4", port=656, connect_to=["peer1", "peer2"])
        tinc = TincManager(config)
        tinc._write_tinc_conf()

        content = (config.network_dir / "tinc.conf").read_text()
        assert "Name = test-node" in content
        assert "Port = 656" in content
        assert "ConnectTo = peer1" in content
        assert "ConnectTo = peer2" in content

    def test_write_tinc_up_down_are_executable(self, tmp_path: Path):
        config = make_config(tmp_path)
        tinc = TincManager(config)
        tinc._write_tinc_up()
        tinc._write_tinc_down()

        for name in ("tinc-up", "tinc-down"):
            script = config.network_dir / name
            assert script.exists()
            assert script.stat().st_mode & 0o111
            assert "10.100.0.1" in script.read_text()

    def test_host_file_includes_address_only_when_public(self, tmp_path: Path):
        config = make_config(tmp_path)
        tinc = TincManager(config)

        public_peer = NodeConfig(
            name="peer-pub",
            internal_ip="10.100.0.2",
            public_ip="5.6.7.8",
            is_publicly_accessible=True,
        )
        private_peer = NodeConfig(
            name="peer-priv",
            internal_ip="10.100.0.3",
            public_ip="9.9.9.9",
            is_publicly_accessible=False,
        )
        config.hosts_dir.mkdir(parents=True)
        tinc._write_host_file(
            public_peer,
            public_key="-----BEGIN RSA PUBLIC KEY-----\nK\n-----END RSA PUBLIC KEY-----",
        )
        tinc._write_host_file(
            private_peer,
            public_key="-----BEGIN RSA PUBLIC KEY-----\nK\n-----END RSA PUBLIC KEY-----",
        )

        pub_content = (config.hosts_dir / "peer-pub").read_text()
        priv_content = (config.hosts_dir / "peer-priv").read_text()
        assert "Address = 5.6.7.8" in pub_content
        assert "Subnet = 10.100.0.2/32" in pub_content
        # A NAT'd peer must not advertise an Address, even if one is known.
        assert "Address" not in priv_content


class TestTincSetup:
    def test_setup_without_private_key_raises(self, tmp_path: Path):
        """GPG-only design: setup must never invent a standalone Tinc key."""
        tinc = TincManager(make_config(tmp_path))
        with pytest.raises(VPNError, match="private key not found"):
            tinc.setup()

    def test_setup_with_existing_key_writes_config(self, tmp_path: Path):
        config = make_config(tmp_path, connect_to=["peer1"])
        config.network_dir.mkdir(parents=True)
        (config.network_dir / "rsa_key.priv").write_text("KEY")

        tinc = TincManager(config)
        tinc.setup()

        assert (config.network_dir / "tinc.conf").exists()
        assert (config.network_dir / "tinc-up").exists()
        assert (config.network_dir / "tinc-down").exists()
        # Local host file created with Subnet/Port header.
        local = (config.hosts_dir / "test-node").read_text()
        assert "Subnet = 10.100.0.1/32" in local

    def test_setup_derives_connect_to_from_public_peers(self, tmp_path: Path):
        config = make_config(tmp_path)
        config.network_dir.mkdir(parents=True)
        (config.network_dir / "rsa_key.priv").write_text("KEY")

        peers = [
            NodeConfig(
                name="pub-peer",
                internal_ip="10.100.0.2",
                public_ip="5.6.7.8",
                is_publicly_accessible=True,
            ),
            NodeConfig(name="nat-peer", internal_ip="10.100.0.3"),
        ]
        tinc = TincManager(config)
        tinc.setup(peers=peers)

        content = (config.network_dir / "tinc.conf").read_text()
        assert "ConnectTo = pub-peer" in content
        assert "ConnectTo = nat-peer" not in content
        # Host files written for every peer, not for self.
        assert (config.hosts_dir / "pub-peer").exists()
        assert (config.hosts_dir / "nat-peer").exists()
