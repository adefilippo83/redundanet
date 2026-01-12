"""Integration tests for Tinc VPN module."""

from pathlib import Path

import pytest

from redundanet.vpn.keys import VPNKeyManager
from redundanet.vpn.tinc import TincConfig, TincManager


class TestTincManagerIntegration:
    """Integration tests for TincManager."""

    def test_setup_creates_config(self, temp_dir: Path):
        """Test that setup creates Tinc configuration files."""
        config = TincConfig(
            config_dir=temp_dir,
            network_name="testnet",
            node_name="test-node",
            vpn_ip="10.100.0.1",
            public_ip="1.2.3.4",
            connect_to=["peer1", "peer2"],
        )
        tinc = TincManager(config)

        # Call setup without peers (no key generation)
        tinc._write_tinc_conf()

        # Check tinc.conf exists
        tinc_conf = config.network_dir / "tinc.conf"
        assert tinc_conf.exists()

        content = tinc_conf.read_text()
        assert "Name = test-node" in content
        assert "ConnectTo = peer1" in content
        assert "ConnectTo = peer2" in content

    def test_write_tinc_up(self, temp_dir: Path):
        """Test generating tinc-up script."""
        config = TincConfig(
            config_dir=temp_dir,
            network_name="testnet",
            node_name="test-node",
            vpn_ip="10.100.0.1",
        )
        tinc = TincManager(config)

        tinc._write_tinc_up()

        # Check script exists
        tinc_up = config.network_dir / "tinc-up"
        assert tinc_up.exists()

        # Check script is executable
        assert tinc_up.stat().st_mode & 0o111

        # Check content
        content = tinc_up.read_text()
        assert "10.100.0.1" in content

    def test_write_tinc_down(self, temp_dir: Path):
        """Test generating tinc-down script."""
        config = TincConfig(
            config_dir=temp_dir,
            network_name="testnet",
            node_name="test-node",
            vpn_ip="10.100.0.1",
        )
        tinc = TincManager(config)

        tinc._write_tinc_down()

        # Check script exists
        tinc_down = config.network_dir / "tinc-down"
        assert tinc_down.exists()

        # Check script is executable
        assert tinc_down.stat().st_mode & 0o111


class TestVPNKeyManagerIntegration:
    """Integration tests for VPNKeyManager."""

    @pytest.mark.skip(reason="Requires tinc to be installed")
    def test_generate_keypair(self, temp_dir: Path):
        """Test generating RSA keypair."""
        config = TincConfig(
            config_dir=temp_dir,
            network_name="testnet",
            node_name="test-node",
        )
        key_manager = VPNKeyManager(config)

        private_key, public_key = key_manager.generate_keypair()

        assert (config.network_dir / "rsa_key.priv").exists()
        assert "-----BEGIN RSA PRIVATE KEY-----" in private_key

    def test_export_import_public_key(self, temp_dir: Path):
        """Test exporting and importing public key."""
        config = TincConfig(
            config_dir=temp_dir,
            network_name="testnet",
            node_name="test-node",
        )

        # Create mock public key for this node
        hosts_dir = config.hosts_dir
        hosts_dir.mkdir(parents=True, exist_ok=True)

        mock_key = "-----BEGIN RSA PUBLIC KEY-----\nMOCKKEY\n-----END RSA PUBLIC KEY-----"
        (hosts_dir / "test-node").write_text(f"Subnet = 10.100.0.1/32\n{mock_key}")

        key_manager = VPNKeyManager(config)

        # Export (exports this node's key - no argument needed)
        exported = key_manager.export_public_key()
        assert "MOCKKEY" in exported

        # Import to another node name
        key_manager.import_public_key("peer-node", exported)
        assert (hosts_dir / "peer-node").exists()

    def test_list_imported_keys(self, temp_dir: Path):
        """Test listing imported keys."""
        config = TincConfig(
            config_dir=temp_dir,
            network_name="testnet",
            node_name="test-node",
        )

        # Create hosts directory with some keys
        hosts_dir = config.hosts_dir
        hosts_dir.mkdir(parents=True, exist_ok=True)

        mock_key = "-----BEGIN RSA PUBLIC KEY-----\nKEY\n-----END RSA PUBLIC KEY-----"
        (hosts_dir / "test-node").write_text(f"Subnet = 10.100.0.1/32\n{mock_key}")
        (hosts_dir / "peer1").write_text(f"Subnet = 10.100.0.2/32\n{mock_key}")
        (hosts_dir / "peer2").write_text(f"Subnet = 10.100.0.3/32\n{mock_key}")

        key_manager = VPNKeyManager(config)

        keys = key_manager.list_imported_keys()
        assert "peer1" in keys
        assert "peer2" in keys
        assert "test-node" not in keys  # Own node excluded

    def test_verify_key(self, temp_dir: Path):
        """Test verifying key format."""
        config = TincConfig(
            config_dir=temp_dir,
            network_name="testnet",
            node_name="test-node",
        )

        hosts_dir = config.hosts_dir
        hosts_dir.mkdir(parents=True, exist_ok=True)

        # Valid key
        mock_key = "-----BEGIN RSA PUBLIC KEY-----\nKEY\n-----END RSA PUBLIC KEY-----"
        (hosts_dir / "valid-node").write_text(f"Subnet = 10.100.0.1/32\n{mock_key}")

        # Invalid key (missing Subnet)
        (hosts_dir / "invalid-node").write_text(mock_key)

        key_manager = VPNKeyManager(config)

        assert key_manager.verify_key("valid-node") is True
        assert key_manager.verify_key("invalid-node") is False
        assert key_manager.verify_key("nonexistent") is False
