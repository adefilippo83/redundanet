"""Unit tests for manifest module."""

from pathlib import Path

import pytest
import yaml

from redundanet.core.exceptions import ManifestError, ValidationError
from redundanet.core.manifest import Manifest, locate_manifest


class TestLocateManifest:
    def test_finds_top_level_manifest(self, tmp_path: Path):
        (tmp_path / "manifest.yaml").write_text("network: {}\n")
        assert locate_manifest(tmp_path) == tmp_path / "manifest.yaml"

    def test_finds_repo_clone_layout(self, tmp_path: Path):
        """A manifest dir that is a repo clone keeps the manifest under manifests/."""
        (tmp_path / "manifests").mkdir()
        (tmp_path / "manifests" / "manifest.yaml").write_text("network: {}\n")
        assert locate_manifest(tmp_path) == tmp_path / "manifests" / "manifest.yaml"

    def test_top_level_wins_when_both_exist(self, tmp_path: Path):
        (tmp_path / "manifest.yaml").write_text("a: 1\n")
        (tmp_path / "manifests").mkdir()
        (tmp_path / "manifests" / "manifest.yaml").write_text("b: 2\n")
        assert locate_manifest(tmp_path) == tmp_path / "manifest.yaml"

    def test_missing_returns_none(self, tmp_path: Path):
        assert locate_manifest(tmp_path) is None


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
                "gpg_key_id": "ABCD1234ABCD1234ABCD1234ABCD1234ABCD1234",  # full fingerprint
                "roles": ["tahoe_introducer", "tahoe_storage"],
                "storage_contribution": "100GB",
            },
            {
                "name": "node2",
                "internal_ip": "192.168.1.11",
                "vpn_ip": "10.100.0.2",
                "gpg_key_id": "1234567812345678123456781234567812345678",  # full fingerprint
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

    def test_same_internal_and_vpn_ip_not_duplicate(self):
        """A node whose internal_ip equals its own vpn_ip is not a duplicate."""
        manifest_data = {
            "network": {
                "name": "test",
                "version": "1.0.0",
                "domain": "test.local",
                "vpn_network": "10.100.0.0/16",
            },
            "nodes": [
                {"name": "node1", "internal_ip": "10.100.0.10", "vpn_ip": "10.100.0.10"},
                {"name": "node2", "internal_ip": "10.100.0.11", "vpn_ip": "10.100.0.11"},
            ],
        }

        manifest = Manifest.from_dict(manifest_data)
        errors = manifest.validate()
        assert not any("Duplicate IP" in e for e in errors)

    def test_introducer_furl(self, manifest_file: Path):
        """Test introducer FURL management."""
        manifest = Manifest.from_file(manifest_file)

        # Update FURL
        new_furl = "pb://newtest@tcp:10.100.0.1:3458/introducer"
        manifest.update_introducer_furl(new_furl)

        assert manifest.introducer_furl == new_furl

    def test_no_introducer_and_no_furl_is_flagged(self, valid_manifest_data: dict):
        """A grid with neither an introducer node nor a FURL cannot bootstrap."""
        data = dict(valid_manifest_data)
        data["introducer_furl"] = None
        data["nodes"] = [
            {
                "name": "node1",
                "internal_ip": "10.100.0.10",
                "roles": ["tahoe_storage"],
            }
        ]
        manifest = Manifest.from_dict(data)
        errors = manifest.validate()
        assert any("No introducer configured" in e for e in errors)

    def test_external_furl_satisfies_introducer_check(self, valid_manifest_data: dict):
        data = dict(valid_manifest_data)
        data["nodes"] = [
            {
                "name": "node1",
                "internal_ip": "10.100.0.10",
                "roles": ["tahoe_storage"],
            }
        ]
        # introducer_furl is set in the fixture -> no bootstrap error.
        manifest = Manifest.from_dict(data)
        errors = manifest.validate()
        assert not any("No introducer configured" in e for e in errors)

    def test_malformed_introducer_furl_is_flagged(self, valid_manifest_data: dict):
        data = dict(valid_manifest_data)
        data["introducer_furl"] = "http://not-a-furl"
        manifest = Manifest.from_dict(data)
        errors = manifest.validate()
        assert any("Invalid introducer_furl" in e for e in errors)

    SECOND_FURL = "pb://second@tcp:10.100.0.2:3458/introducer"

    @staticmethod
    def _with_second_introducer(data: dict, furl: str | None) -> dict:
        """The fixture's node2 promoted to an introducer, optionally publishing a FURL."""
        data = dict(data)
        nodes = [dict(n) for n in data["nodes"]]
        nodes[1]["roles"] = ["tahoe_introducer", "tahoe_storage"]
        if furl is not None:
            nodes[1]["introducer_furl"] = furl
        data["nodes"] = nodes
        return data

    def test_top_level_furl_plus_second_node_furl_is_clean(self, valid_manifest_data: dict):
        # node1's FURL is the top-level one (the historical hub layout); node2
        # publishes its own. Nothing to warn about.
        data = self._with_second_introducer(valid_manifest_data, self.SECOND_FURL)
        manifest = Manifest.from_dict(data)
        result = manifest.validate_detailed()
        assert not result.errors
        assert not any("introducer_furl" in w for w in result.warnings)
        # Clients get the primary first, then the second introducer.
        assert manifest.introducer_furls == [data["introducer_furl"], self.SECOND_FURL]
        # The node's FURL survives a round trip through to_dict (what gets saved).
        assert manifest.to_dict()["nodes"][1]["introducer_furl"] == self.SECOND_FURL

    def test_second_introducer_without_a_furl_warns(self, valid_manifest_data: dict):
        data = self._with_second_introducer(valid_manifest_data, None)
        result = Manifest.from_dict(data).validate_detailed()
        assert any("node2 has no introducer_furl" in w for w in result.warnings)

    def test_sole_introducer_covered_by_top_level_furl_is_silent(self, valid_manifest_data: dict):
        # One introducer node, no per-node FURL, top-level FURL set: today's layout.
        result = Manifest.from_dict(valid_manifest_data).validate_detailed()
        assert not result.errors
        assert not any("introducer_furl" in w for w in result.warnings)

    def test_malformed_node_introducer_furl_is_an_error(self, valid_manifest_data: dict):
        data = self._with_second_introducer(valid_manifest_data, "http://not-a-furl")
        errors = Manifest.from_dict(data).validate()
        assert any("Invalid introducer_furl on node node2" in e for e in errors)

    def test_short_gpg_key_ids_are_blocking_errors(self, valid_manifest_data: dict):
        """Short ids cannot be fetched/matched by the runtime (fail-closed
        keyserver client), so a manifest carrying one is broken — ERROR."""
        data = dict(valid_manifest_data)
        data["nodes"] = [dict(n) for n in data["nodes"]]
        data["nodes"][0]["gpg_key_id"] = "ABCD1234"  # short id
        result = Manifest.from_dict(data).validate_detailed()

        assert any("short GPG key ids" in e for e in result.errors)
        assert not any("short GPG key ids" in w for w in result.warnings)

    def test_validate_detailed_splits_errors_from_warnings(self, valid_manifest_data: dict):
        # A duplicate IP is blocking; under-provisioning stays advisory.
        data = dict(valid_manifest_data)
        data["nodes"] = [dict(n) for n in data["nodes"]]
        data["nodes"][1]["vpn_ip"] = data["nodes"][0]["vpn_ip"]  # force a dup IP
        result = Manifest.from_dict(data).validate_detailed()

        assert any("Duplicate IP" in e for e in result.errors)
        assert any("Not enough storage nodes" in w for w in result.warnings)
        # Advisory items must NOT be classified as blocking errors.
        assert not any("Not enough storage nodes" in e for e in result.errors)
        # The flat validate() stays a superset for backward compatibility.
        flat = Manifest.from_dict(data).validate()
        assert set(result.errors + result.warnings) == set(flat)

    def test_write_spof_is_warning_not_error(self, valid_manifest_data: dict):
        # shares_happy == storage-node count: no write-redundancy headroom. This
        # is the live production shape (1-of-2), so it must warn, never block.
        data = dict(valid_manifest_data)
        data["network"] = {
            **data["network"],
            "tahoe": {"shares_needed": 1, "shares_happy": 2, "shares_total": 2},
        }
        result = Manifest.from_dict(data).validate_detailed()

        assert result.errors == []
        assert any("no write-redundancy headroom" in w for w in result.warnings)
        assert not any("Not enough storage nodes" in w for w in result.warnings)
