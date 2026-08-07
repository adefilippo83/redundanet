"""Unit tests for the manifest PR validation script (.github/scripts/validate_pr.py)."""

from __future__ import annotations

import sys
from pathlib import Path

import yaml
from hypothesis import given
from hypothesis import strategies as st

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / ".github" / "scripts"))

import validate_pr  # noqa: E402


def write_manifest(tmp_path: Path, data: dict) -> str:
    path = tmp_path / "manifest.yaml"
    path.write_text(yaml.dump(data))
    return str(path)


def valid_manifest() -> dict:
    return {
        "network": {
            "name": "redundanet",
            "version": "2.0.0",
            "domain": "redundanet.local",
            "vpn_network": "10.100.0.0/16",
        },
        "nodes": [
            {
                "name": "node-a",
                "internal_ip": "10.100.0.10",
                "vpn_ip": "10.100.0.10",
                "gpg_key_id": "1234567890ABCDEF1234567890ABCDEF12345678",
                "status": "pending",
                "roles": ["tinc_vpn", "tahoe_storage"],
            },
        ],
    }


class TestValidManifest:
    def test_passes_with_no_errors(self, tmp_path: Path):
        errors, warnings = validate_pr.validate(write_manifest(tmp_path, valid_manifest()))
        assert errors == []
        assert warnings == []

    def test_short_key_id_is_warning_not_error(self, tmp_path: Path):
        manifest = valid_manifest()
        manifest["nodes"][0]["gpg_key_id"] = "DEADBEEFCAFE1234"
        errors, warnings = validate_pr.validate(write_manifest(tmp_path, manifest))
        assert errors == []
        assert len(warnings) == 1
        assert "short key id" in warnings[0]


class TestErrors:
    def test_missing_file(self, tmp_path: Path):
        errors, _ = validate_pr.validate(str(tmp_path / "nope.yaml"))
        assert any("not found" in e for e in errors)

    def test_non_mapping_document(self, tmp_path: Path):
        path = tmp_path / "manifest.yaml"
        path.write_text("- just\n- a\n- list\n")
        errors, _ = validate_pr.validate(str(path))
        assert any("mapping" in e for e in errors)

    def test_missing_gpg_key_id(self, tmp_path: Path):
        manifest = valid_manifest()
        del manifest["nodes"][0]["gpg_key_id"]
        errors, _ = validate_pr.validate(write_manifest(tmp_path, manifest))
        assert any("missing 'gpg_key_id'" in e for e in errors)

    def test_invalid_gpg_key_id(self, tmp_path: Path):
        manifest = valid_manifest()
        manifest["nodes"][0]["gpg_key_id"] = "not-hex!"
        errors, _ = validate_pr.validate(write_manifest(tmp_path, manifest))
        assert any("invalid gpg_key_id" in e for e in errors)

    def test_vpn_ip_outside_network(self, tmp_path: Path):
        manifest = valid_manifest()
        manifest["nodes"][0]["vpn_ip"] = "192.168.1.1"
        manifest["nodes"][0]["internal_ip"] = "192.168.1.1"
        errors, _ = validate_pr.validate(write_manifest(tmp_path, manifest))
        assert any("outside" in e for e in errors)

    def test_duplicate_ip_across_nodes(self, tmp_path: Path):
        manifest = valid_manifest()
        manifest["nodes"].append(
            {
                "name": "node-b",
                "internal_ip": "10.100.0.10",  # same as node-a
                "gpg_key_id": "ABCDEF1234567890ABCDEF1234567890ABCDEF12",
            }
        )
        errors, _ = validate_pr.validate(write_manifest(tmp_path, manifest))
        assert any("used by multiple nodes" in e for e in errors)

    def test_duplicate_node_name(self, tmp_path: Path):
        manifest = valid_manifest()
        duplicate = dict(manifest["nodes"][0])
        duplicate["internal_ip"] = "10.100.0.11"
        duplicate["vpn_ip"] = "10.100.0.11"
        manifest["nodes"].append(duplicate)
        errors, _ = validate_pr.validate(write_manifest(tmp_path, manifest))
        assert any("Duplicate node name" in e for e in errors)

    def test_invalid_role_and_status(self, tmp_path: Path):
        manifest = valid_manifest()
        manifest["nodes"][0]["roles"] = ["warp_drive"]
        manifest["nodes"][0]["status"] = "sleeping"
        errors, _ = validate_pr.validate(write_manifest(tmp_path, manifest))
        assert any("invalid role" in e for e in errors)
        assert any("invalid status" in e for e in errors)


class TestKeyIdPredicate:
    @given(st.text(max_size=60))
    def test_never_crashes(self, value: str):
        validate_pr._is_gpg_key_id(value)

    @given(st.text(alphabet="0123456789abcdefABCDEF", min_size=1, max_size=50))
    def test_hex_accepted_only_at_valid_lengths(self, value: str):
        assert validate_pr._is_gpg_key_id(value) == (len(value) in (8, 16, 40))
