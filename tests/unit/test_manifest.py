"""Unit tests for manifest module."""

from pathlib import Path

import pytest
import yaml

from redundanet.core.exceptions import ManifestError, ValidationError
from redundanet.core.manifest import Manifest


@pytest.fixture
def valid_manifest_data() -> dict:
    """Return valid manifest data for testing."""
    return {
        "network": {
            "name": "test-network",
            "version": "1.0.0",
            "domain": "test.local",
            "vpn_network": "10.100.0.0/16",
            "tahoe": {
                "shares_needed": 3,
                "shares_happy": 7,
                "shares_total": 10,
            },
        },
        "introducer_furl": "pb://test@tcp:10.100.0.1:3458/introducer",
        "nodes": [
            {
                "name": "node1",
                "internal_ip": "192.168.1.10",
                "vpn_ip": "10.100.0.1",
                "public_ip": "1.2.3.4",
                "gpg_key_id": "ABCD1234",  # Valid 8-char hex
                "roles": ["tahoe_introducer", "tahoe_storage"],
                "storage_contribution": "100GB",
            },
            {
                "name": "node2",
                "internal_ip": "192.168.1.11",
                "vpn_ip": "10.100.0.2",
                "gpg_key_id": "12345678",  # Valid 8-char hex
                "roles": ["tahoe_storage", "tahoe_client"],
                "storage_contribution": "500GB",
            },
        ],
    }


@pytest.fixture
def manifest_file(tmp_path: Path, valid_manifest_data: dict) -> Path:
    """Create a sample manifest file for testing."""
    manifest_path = tmp_path / "manifest.yaml"
    with open(manifest_path, "w") as f:
        yaml.dump(valid_manifest_data, f)
    return manifest_path


class TestManifest:
    """Tests for Manifest class."""

    def test_load_from_file(self, manifest_file: Path):
        """Test loading a valid manifest file."""
        manifest = Manifest.from_file(manifest_file)

        assert manifest.network.name == "test-network"
        assert len(manifest.nodes) == 2

    def test_load_nonexistent_file(self, tmp_path: Path):
        """Test loading a non-existent manifest file."""
        with pytest.raises(ManifestError):
            Manifest.from_file(tmp_path / "nonexistent.yaml")

    def test_from_dict(self, valid_manifest_data: dict):
        """Test creating manifest from dictionary."""
        manifest = Manifest.from_dict(valid_manifest_data)

        assert manifest.network.name == "test-network"
        assert manifest.network.version == "1.0.0"
        assert len(manifest.nodes) == 2

    def test_get_node(self, manifest_file: Path):
        """Test getting a specific node from manifest."""
        manifest = Manifest.from_file(manifest_file)

        node = manifest.get_node("node1")
        assert node is not None
        assert node.name == "node1"
        assert node.vpn_ip == "10.100.0.1"

    def test_get_nonexistent_node(self, manifest_file: Path):
        """Test getting a non-existent node."""
        manifest = Manifest.from_file(manifest_file)

        node = manifest.get_node("nonexistent")
        assert node is None

    def test_get_nodes_by_role(self, manifest_file: Path):
        """Test filtering nodes by role."""
        manifest = Manifest.from_file(manifest_file)

        storage_nodes = manifest.get_nodes_by_role("tahoe_storage")
        assert len(storage_nodes) == 2

        introducer_nodes = manifest.get_nodes_by_role("tahoe_introducer")
        assert len(introducer_nodes) == 1
        assert introducer_nodes[0].name == "node1"

        client_nodes = manifest.get_nodes_by_role("tahoe_client")
        assert len(client_nodes) == 1
        assert client_nodes[0].name == "node2"

    def test_to_dict(self, manifest_file: Path):
        """Test converting manifest to dictionary."""
        manifest = Manifest.from_file(manifest_file)
        data = manifest.to_dict()

        assert data["network"]["name"] == "test-network"
        assert len(data["nodes"]) == 2

    def test_save_manifest(self, tmp_path: Path, valid_manifest_data: dict):
        """Test saving manifest to file."""
        manifest = Manifest.from_dict(valid_manifest_data)
        save_path = tmp_path / "saved_manifest.yaml"

        manifest.save(save_path)

        assert save_path.exists()

        # Reload and verify
        reloaded = Manifest.from_file(save_path)
        assert reloaded.network.name == "test-network"
        assert len(reloaded.nodes) == 2

    def test_get_network_config(self, manifest_file: Path):
        """Test getting network configuration."""
        manifest = Manifest.from_file(manifest_file)

        assert manifest.network.name == "test-network"
        assert manifest.network.version == "1.0.0"
        assert manifest.network.domain == "test.local"

    def test_get_tahoe_config(self, manifest_file: Path):
        """Test getting Tahoe configuration."""
        manifest = Manifest.from_file(manifest_file)

        assert manifest.network.tahoe.shares_needed == 3
        assert manifest.network.tahoe.shares_happy == 7
        assert manifest.network.tahoe.shares_total == 10

    def test_validate_manifest(self, manifest_file: Path):
        """Test manifest validation."""
        manifest = Manifest.from_file(manifest_file)

        # Should return warnings but not raise for this manifest
        errors = manifest.validate()
        # The sample manifest has fewer storage nodes than shares_happy
        assert isinstance(errors, list)

    def test_validate_invalid_schema(self, tmp_path: Path):
        """Test validation of manifest with invalid schema."""
        invalid_manifest = {
            "network": {
                "name": "test",
                # missing required fields
            },
            "nodes": [],
        }

        manifest_path = tmp_path / "invalid.yaml"
        with open(manifest_path, "w") as f:
            yaml.dump(invalid_manifest, f)

        with pytest.raises(ValidationError):
            Manifest.from_file(manifest_path)

    def test_detect_duplicate_names(self, tmp_path: Path):
        """Test detection of duplicate node names."""
        manifest_data = {
            "network": {
                "name": "test",
                "version": "1.0.0",
                "domain": "test.local",
                "vpn_network": "10.100.0.0/16",
            },
            "nodes": [
                {"name": "node1", "internal_ip": "192.168.1.10"},
                {"name": "node1", "internal_ip": "192.168.1.11"},  # duplicate name
            ],
        }

        manifest = Manifest.from_dict(manifest_data)
        errors = manifest.validate()
        assert any("Duplicate node names" in e for e in errors)

    def test_detect_duplicate_ips(self, tmp_path: Path):
        """Test detection of duplicate IP addresses."""
        manifest_data = {
            "network": {
                "name": "test",
                "version": "1.0.0",
                "domain": "test.local",
                "vpn_network": "10.100.0.0/16",
            },
            "nodes": [
                {"name": "node1", "internal_ip": "192.168.1.10", "vpn_ip": "10.100.0.1"},
                {
                    "name": "node2",
                    "internal_ip": "192.168.1.10",
                    "vpn_ip": "10.100.0.2",
                },  # duplicate IP
            ],
        }

        manifest = Manifest.from_dict(manifest_data)
        errors = manifest.validate()
        assert any("Duplicate IP" in e for e in errors)

    def test_introducer_furl(self, manifest_file: Path):
        """Test introducer FURL management."""
        manifest = Manifest.from_file(manifest_file)

        # Update FURL
        new_furl = "pb://newtest@tcp:10.100.0.1:3458/introducer"
        manifest.update_introducer_furl(new_furl)

        assert manifest.introducer_furl == new_furl
