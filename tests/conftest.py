"""Pytest configuration and fixtures for RedundaNet tests."""

import os
import tempfile
from collections.abc import Generator
from pathlib import Path

import pytest
import yaml


@pytest.fixture
def temp_dir() -> Generator[Path, None, None]:
    """Create a temporary directory for tests."""
    with tempfile.TemporaryDirectory() as tmpdir:
        yield Path(tmpdir)


@pytest.fixture
def sample_manifest_data() -> dict:
    """Canonical, schema-valid manifest data for testing.

    Keep this in sync with manifests/schema.json / core.manifest.MANIFEST_SCHEMA —
    it is the reference shape tests should build on.
    """
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
                "reserved_space": "1G",
            },
        },
        "introducer_furl": "pb://abc234def@tcp:10.100.0.1:3458/introducerswiss",
        "nodes": [
            {
                "name": "node1",
                "internal_ip": "10.100.0.1",
                "vpn_ip": "10.100.0.1",
                "public_ip": "203.0.113.1",
                "gpg_key_id": "1234567890ABCDEF1234567890ABCDEF12345678",
                "status": "active",
                "roles": ["tinc_vpn", "tahoe_introducer", "tahoe_storage"],
                "storage_contribution": "100GB",
                "is_publicly_accessible": True,
            },
            {
                "name": "node2",
                "internal_ip": "10.100.0.2",
                "vpn_ip": "10.100.0.2",
                "gpg_key_id": "ABCDEF1234567890ABCDEF1234567890ABCDEF12",
                "status": "pending",
                "roles": ["tinc_vpn", "tahoe_storage", "tahoe_client"],
                "storage_contribution": "500GB",
            },
        ],
    }


@pytest.fixture
def sample_manifest_file(temp_dir: Path, sample_manifest_data: dict) -> Path:
    """Create a sample manifest file for testing."""
    manifest_path = temp_dir / "manifest.yaml"
    with open(manifest_path, "w") as f:
        yaml.dump(sample_manifest_data, f)
    return manifest_path


@pytest.fixture
def mock_env_vars(monkeypatch: pytest.MonkeyPatch) -> None:
    """Set up mock environment variables."""
    monkeypatch.setenv("REDUNDANET_NODE_NAME", "test-node")
    monkeypatch.setenv("REDUNDANET_INTERNAL_VPN_IP", "10.100.0.10")
    monkeypatch.setenv("REDUNDANET_LOG_LEVEL", "DEBUG")


@pytest.fixture
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Clear RedundaNet environment variables."""
    for key in list(os.environ.keys()):
        if key.startswith("REDUNDANET_"):
            monkeypatch.delenv(key, raising=False)
