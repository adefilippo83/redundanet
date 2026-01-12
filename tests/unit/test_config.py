"""Unit tests for configuration module."""

from pathlib import Path

import pytest

from redundanet.core.config import (
    AppSettings,
    NetworkConfig,
    NodeConfig,
    NodeRole,
    TahoeConfig,
)


class TestTahoeConfig:
    """Tests for TahoeConfig model."""

    def test_default_values(self):
        """Test default Tahoe configuration values."""
        config = TahoeConfig()
        assert config.shares_needed == 3
        assert config.shares_happy == 7
        assert config.shares_total == 10
        assert config.reserved_space == "50G"

    def test_custom_values(self):
        """Test custom Tahoe configuration values."""
        config = TahoeConfig(
            shares_needed=2,
            shares_happy=5,
            shares_total=8,
            reserved_space="100G",
        )
        assert config.shares_needed == 2
        assert config.shares_happy == 5
        assert config.shares_total == 8
        assert config.reserved_space == "100G"

    def test_validation_shares_order(self):
        """Test that shares_needed <= shares_happy <= shares_total."""
        config = TahoeConfig(shares_needed=3, shares_happy=7, shares_total=10)
        assert config.shares_needed <= config.shares_happy <= config.shares_total

    def test_invalid_shares_order(self):
        """Test that invalid shares order raises error."""
        with pytest.raises(ValueError):
            TahoeConfig(shares_needed=5, shares_happy=3, shares_total=10)


class TestNodeConfig:
    """Tests for NodeConfig model."""

    def test_minimal_config(self):
        """Test node config with minimal required fields."""
        config = NodeConfig(
            name="test-node",
            internal_ip="192.168.1.10",
        )
        assert config.name == "test-node"
        assert config.internal_ip == "192.168.1.10"
        # vpn_ip defaults to internal_ip
        assert config.vpn_ip == "192.168.1.10"
        assert config.public_ip is None
        assert config.roles == []

    def test_full_config(self):
        """Test node config with all fields."""
        config = NodeConfig(
            name="test-node",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
            public_ip="1.2.3.4",
            gpg_key_id="ABCD1234",  # Valid 8-char hex
            roles=[NodeRole.TAHOE_INTRODUCER, NodeRole.TAHOE_STORAGE],
            storage_contribution="500GB",
            storage_allocation="1TB",
        )
        assert config.name == "test-node"
        assert config.vpn_ip == "10.100.0.1"
        assert config.public_ip == "1.2.3.4"
        assert NodeRole.TAHOE_INTRODUCER in config.roles
        assert config.storage_contribution == "500GB"

    def test_invalid_gpg_key_id(self):
        """Test that invalid GPG key ID raises error."""
        with pytest.raises(ValueError):
            NodeConfig(
                name="test-node",
                internal_ip="192.168.1.10",
                gpg_key_id="INVALID",  # Not hex
            )

    def test_has_role(self):
        """Test has_role method."""
        config = NodeConfig(
            name="test",
            internal_ip="192.168.1.10",
            roles=[NodeRole.TAHOE_STORAGE],
        )
        assert config.has_role(NodeRole.TAHOE_STORAGE)
        assert not config.has_role(NodeRole.TAHOE_INTRODUCER)


class TestNetworkConfig:
    """Tests for NetworkConfig model."""

    def test_default_values(self):
        """Test network config with defaults."""
        config = NetworkConfig(
            name="test-network",
            version="1.0.0",
        )
        assert config.name == "test-network"
        assert config.version == "1.0.0"
        assert config.domain == "redundanet.local"
        assert config.vpn_network == "10.100.0.0/16"
        assert isinstance(config.tahoe, TahoeConfig)

    def test_custom_values(self):
        """Test network config with custom values."""
        config = NetworkConfig(
            name="my-network",
            version="2.0.0",
            domain="mynet.local",
            vpn_network="172.16.0.0/16",
            tahoe=TahoeConfig(shares_needed=2),
        )
        assert config.domain == "mynet.local"
        assert config.vpn_network == "172.16.0.0/16"
        assert config.tahoe.shares_needed == 2


class TestAppSettings:
    """Tests for AppSettings model."""

    def test_default_paths(self):
        """Test default application paths."""
        settings = AppSettings()
        assert settings.config_dir == Path("/etc/redundanet")
        assert settings.data_dir == Path("/var/lib/redundanet")
        assert settings.log_level == "INFO"
        assert settings.debug is False

    def test_env_override(self, monkeypatch):
        """Test environment variable overrides."""
        monkeypatch.setenv("REDUNDANET_LOG_LEVEL", "DEBUG")
        monkeypatch.setenv("REDUNDANET_DEBUG", "true")

        settings = AppSettings()
        assert settings.log_level == "DEBUG"
        assert settings.debug is True

    def test_custom_paths(self, tmp_path):
        """Test custom configuration paths."""
        settings = AppSettings(
            config_dir=tmp_path / "config",
            data_dir=tmp_path / "data",
        )
        assert settings.config_dir == tmp_path / "config"
        assert settings.data_dir == tmp_path / "data"
