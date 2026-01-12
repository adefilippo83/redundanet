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
    """Return sample manifest data for testing."""
    return {
        "network": {
            "name": "test-network",
            "version": "1.0.0",
            "domain": "test.local",
            "vpn_network": "10.100.0.0/16",
        },
        "tahoe": {
            "shares_needed": 3,
            "shares_happy": 7,
            "shares_total": 10,
            "introducer_furl": "pb://test@tcp:10.100.0.1:3458/introducer",
        },
        "nodes": [
            {
                "name": "node1",
                "internal_ip": "192.168.1.10",
                "vpn_ip": "10.100.0.1",
                "public_ip": "1.2.3.4",
                "gpg_key_id": "ABCD1234",
                "roles": ["introducer", "storage"],
                "storage_contribution": "100GB",
            },
            {
                "name": "node2",
                "internal_ip": "192.168.1.11",
                "vpn_ip": "10.100.0.2",
                "gpg_key_id": "EFGH5678",
                "roles": ["storage", "client"],
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
